// Package sse lê e encaminha eventos Server-Sent Events com limites explícitos.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const MaxLineBytes = 1 << 20

// Event representa um evento SSE já separado, com buffers próprios.
type Event struct {
	Data string
	Raw  []byte
}

// RawEvent é um frame SSE sem cópia. Seus slices são válidos até a próxima
// chamada a Reader.NextRaw ou Reader.Reset. Use-o quando o consumidor encaminha
// cada frame imediatamente, como no streaming do gateway.
type RawEvent struct {
	Data []byte
	Raw  []byte
}

// Reader lê eventos sem ultrapassar o limite por linha. Os buffers internos são
// reutilizados por NextRaw para evitar uma alocação por chunk no caminho SSE.
type Reader struct {
	r    *bufio.Reader
	line []byte
	raw  []byte
	data []byte
}

// NewReader cria um leitor SSE.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Reset reutiliza os buffers do leitor para um novo stream.
func (r *Reader) Reset(input io.Reader) {
	r.r.Reset(input)
	r.line = r.line[:0]
	r.raw = r.raw[:0]
	r.data = r.data[:0]
}

// NextRaw lê o próximo evento. Data e Raw pertencem ao Reader e só permanecem
// válidos até a próxima chamada a NextRaw ou Reset.
func (r *Reader) NextRaw() (RawEvent, error) {
	r.raw = r.raw[:0]
	r.data = r.data[:0]
	for {
		line, err := r.readLine()
		trim := trimLineEnding(line)
		if len(line) > 0 {
			r.raw = append(r.raw, line...)
			if bytes.HasPrefix(trim, []byte("data:")) {
				value := trim[len("data:"):]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if len(r.data) > 0 {
					r.data = append(r.data, '\n')
				}
				r.data = append(r.data, value...)
			}
		}
		if err != nil && err != io.EOF {
			return RawEvent{}, err
		}
		if len(trim) == 0 {
			if len(r.raw) == 0 {
				return RawEvent{}, io.EOF
			}
			return RawEvent{Data: r.data, Raw: r.raw}, nil
		}
		if err == io.EOF {
			if len(r.raw) == 0 {
				return RawEvent{}, io.EOF
			}
			return RawEvent{Data: r.data, Raw: r.raw}, nil
		}
	}
}

func (r *Reader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	for {
		fragment, err := r.r.ReadSlice('\n')
		if len(r.line)+len(fragment) > MaxLineBytes {
			return nil, fmt.Errorf("sse: linha excede %d bytes", MaxLineBytes)
		}
		r.line = append(r.line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return r.line, err
	}
}

func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// Next mantém a API que entrega strings/slices independentes para consumidores
// que retêm eventos. No caminho quente, prefira NextRaw e consuma antes do next.
func (r *Reader) Next() (Event, error) {
	event, err := r.NextRaw()
	if err != nil {
		return Event{}, err
	}
	return Event{Data: string(event.Data), Raw: bytes.Clone(event.Raw)}, nil
}
func readLineBounded(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(out)+len(frag) > MaxLineBytes {
			return nil, fmt.Errorf("sse: linha excede %d bytes", MaxLineBytes)
		}
		out = append(out, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return out, err
	}
}

// IsDone informa se o evento é o marcador final da API OpenAI.
func IsDone(e Event) bool { return strings.TrimSpace(e.Data) == "[DONE]" }

// IsDoneRaw verifica o marcador terminal sem converter o payload para string.
func IsDoneRaw(e RawEvent) bool { return bytes.Equal(bytes.TrimSpace(e.Data), []byte("[DONE]")) }

// LastUsage extrai usage do último evento de dados; eventos vazios e [DONE] são ignorados.
func LastUsage(r io.Reader) (*Usage, error) {
	sr := NewReader(r)
	var last *Usage
	for {
		e, err := sr.NextRaw()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(e.Data) == 0 || IsDoneRaw(e) {
			continue
		}
		var v struct {
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal(e.Data, &v) == nil && v.Usage != nil {
			u := *v.Usage
			last = &u
		}
	}
	return last, nil
}

// PeekFirstDataEvent ignora frames de comentário/vazios antes do primeiro data:.
// Todos os bytes lidos, inclusive esses frames, continuam disponíveis em Reader.
func PeekFirstDataEvent(r io.Reader) (FirstEvent, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var captured bytes.Buffer
	for {
		first, err := PeekFirstEvent(br)
		if err != nil {
			return FirstEvent{}, err
		}
		captured.Write(first.Event.Raw)
		if first.Event.Data != "" {
			prefix := append([]byte(nil), captured.Bytes()...)
			return FirstEvent{Event: first.Event, Reader: io.MultiReader(bytes.NewReader(prefix), br)}, nil
		}
	}
}

// Usage é o subconjunto de tokens publicado por APIs OpenAI-compatíveis.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// FirstEvent guarda o primeiro evento e um Reader que reproduz todos os bytes originais.
type FirstEvent struct {
	Event  Event
	Reader io.Reader
}

// PeekFirstEvent lê até o primeiro evento sem perder bytes do fluxo.
func PeekFirstEvent(r io.Reader) (FirstEvent, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var captured bytes.Buffer
	var data []string
	for {
		line, err := readLineBounded(br)
		if len(line) > 0 {
			captured.Write(line)
			trim := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			if strings.HasPrefix(trim, "data:") {
				v := strings.TrimPrefix(trim, "data:")
				if strings.HasPrefix(v, " ") {
					v = v[1:]
				}
				data = append(data, v)
			}
		}
		if err != nil && err != io.EOF {
			return FirstEvent{}, err
		}
		if len(line) == 0 || bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			if captured.Len() == 0 {
				return FirstEvent{}, io.EOF
			}
			break
		}
		if err == io.EOF {
			if captured.Len() == 0 {
				return FirstEvent{}, io.EOF
			}
			break
		}
	}
	prefix := append([]byte(nil), captured.Bytes()...)
	return FirstEvent{Event: Event{Data: strings.Join(data, "\n"), Raw: prefix}, Reader: io.MultiReader(bytes.NewReader(prefix), br)}, nil
}
