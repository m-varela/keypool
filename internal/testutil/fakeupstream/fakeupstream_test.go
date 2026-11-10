package fakeupstream

import (
	"io"
	"net/http"
	"testing"
)

func TestScriptedResponseByAuthorization(t *testing.T) {
	s := New()
	defer s.Close()
	s.Enqueue("Bearer secret", JSON(http.StatusOK, `{"ok":true}`))
	req, _ := http.NewRequest(http.MethodGet, s.URL(), nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"ok":true}` {
		t.Fatalf("%s", b)
	}
	if s.Requests() != 1 {
		t.Fatal(s.Requests())
	}
}
