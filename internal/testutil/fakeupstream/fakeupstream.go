// Package fakeupstream cria servidores HTTP locais programáveis para testes offline.
package fakeupstream

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Response descreve uma resposta programada por credencial.
type Response struct {
	Status      int
	Header      http.Header
	Body        []byte
	Chunks      [][]byte
	ChunkDelay  time.Duration
	ChunkDelays []time.Duration
	DropAfter   int
}

// Server oferece um httptest.Server com filas independentes por Authorization.
type Server struct {
	HTTP     *httptest.Server
	mu       sync.Mutex
	scripts  map[string][]Response
	requests int
}

// New cria o servidor fake.
func New() *Server {
	s := &Server{scripts: map[string][]Response{}}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// URL retorna a URL local.
func (s *Server) URL() string { return s.HTTP.URL }

// Client retorna o cliente do httptest.
func (s *Server) Client() *http.Client { return s.HTTP.Client() }

// Close encerra o servidor.
func (s *Server) Close() { s.HTTP.Close() }

// Enqueue adiciona uma resposta para o valor exato do cabeçalho Authorization.
func (s *Server) Enqueue(authorization string, r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[authorization] = append(s.scripts[authorization], r)
}

// Requests devolve o número de solicitações recebidas.
func (s *Server) Requests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.requests++
	q := s.scripts[auth]
	if len(q) == 0 {
		s.mu.Unlock()
		http.Error(w, "no scripted response for "+safeLabel(auth), http.StatusNotFound)
		return
	}
	step := q[0]
	s.scripts[auth] = q[1:]
	s.mu.Unlock()
	for k, vs := range step.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := step.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(step.Chunks) == 0 {
		_, _ = w.Write(step.Body)
		return
	}
	flusher, _ := w.(http.Flusher)
	for i, c := range step.Chunks {
		if step.DropAfter > 0 && i >= step.DropAfter {
			return
		}
		delay := step.ChunkDelay
		if i < len(step.ChunkDelays) {
			delay = step.ChunkDelays[i]
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if _, e := w.Write(c); e != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}
func safeLabel(s string) string {
	if s == "" {
		return "missing"
	}
	if strings.HasPrefix(s, "Bearer ") {
		return "bearer-[redacted]"
	}
	return fmt.Sprintf("auth-[%d-bytes]", len(s))
}

// JSON cria uma resposta JSON com status escolhido.
func JSON(status int, body string) Response {
	return Response{Status: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)}
}

// ReadRequestBody é utilitário de teste; nunca deve ser chamado por código de produção.
func ReadRequestBody(r *http.Request) []byte { b, _ := io.ReadAll(r.Body); return b }
