package sse

import (
	"io"
	"strings"
	"testing"
)

func TestLastUsageUsesLastEvent(t *testing.T) {
	src := "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
	u, e := LastUsage(strings.NewReader(src))
	if e != nil || u == nil || u.TotalTokens != 10 {
		t.Fatalf("%+v %v", u, e)
	}
}
func TestNextRawPreservesCRLFAndMultipleDataLines(t *testing.T) {
	src := "event: message\r\ndata: first\r\ndata: second\r\n\r\ndata: [DONE]\r\n\r\n"
	reader := NewReader(strings.NewReader(src))
	first, err := reader.NextRaw()
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != "first\nsecond" || string(first.Raw) != "event: message\r\ndata: first\r\ndata: second\r\n\r\n" {
		t.Fatalf("unexpected raw event: data=%q raw=%q", first.Data, first.Raw)
	}
	second, err := reader.NextRaw()
	if err != nil {
		t.Fatal(err)
	}
	if !IsDoneRaw(second) {
		t.Fatalf("terminal marker not recognized: %q", second.Data)
	}
	if _, err = reader.NextRaw(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestNextReturnsIndependentEventBuffers(t *testing.T) {
	reader := NewReader(strings.NewReader("data: first\n\ndata: second\n\n"))
	first, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Data != "first" || string(first.Raw) != "data: first\n\n" {
		t.Fatalf("first event mutated after next: %+v", first)
	}
	if second.Data != "second" {
		t.Fatalf("second event=%+v", second)
	}
}

func TestPeekFirstEventDoesNotConsumeBytes(t *testing.T) {
	src := "data: {\"error\":{\"message\":\"bad\"}}\n\ndata: [DONE]\n\n"
	p, e := PeekFirstEvent(strings.NewReader(src))
	if e != nil {
		t.Fatal(e)
	}
	got, _ := io.ReadAll(p.Reader)
	if string(got) != src || !strings.Contains(p.Event.Data, "error") {
		t.Fatalf("%q", got)
	}
}
func TestLineLimit(t *testing.T) {
	s := "data: " + strings.Repeat("x", MaxLineBytes+1) + "\n\n"
	if _, e := NewReader(strings.NewReader(s)).Next(); e == nil {
		t.Fatal("expected line limit")
	}
}
func FuzzSSE(f *testing.F) {
	f.Add("data: [DONE]\n\n")
	f.Fuzz(func(t *testing.T, s string) { _, _ = NewReader(strings.NewReader(s)).Next() })
}

func TestPeekFirstDataEventPreservesLeadingCommentsAndCRLF(t *testing.T) {
	src := ": keep-alive\r\n\r\ndata: {\"choices\":[]}\r\n\r\ndata: [DONE]\r\n\r\n"
	p, err := PeekFirstDataEvent(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Event.Data, `"choices"`) {
		t.Fatalf("first data event=%q", p.Event.Data)
	}
	got, err := io.ReadAll(p.Reader)
	if err != nil || string(got) != src {
		t.Fatalf("stream bytes changed: got=%q err=%v", got, err)
	}
}

func BenchmarkSSEForward(b *testing.B) {
	var frame strings.Builder
	for i := 0; i < 16; i++ {
		frame.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"benchmark chunk\"}}]}\n\n")
	}
	frame.WriteString("data: [DONE]\n\n")
	payload := frame.String()
	source := strings.NewReader(payload)
	reader := NewReader(source)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		source.Reset(payload)
		reader.Reset(source)
		for {
			event, err := reader.NextRaw()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			if _, err = io.Discard.Write(event.Raw); err != nil {
				b.Fatal(err)
			}
		}
	}
}
