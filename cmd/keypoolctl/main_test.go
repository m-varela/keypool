package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportEnvUsesSharedFromEnvOrderingAndDeduplication(t *testing.T) {
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret string `json:"secret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body.Secret)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "keys.env")
	content := "GROQ_API_KEYS=alpha,beta gamma\nGROQ_API_KEY=beta\nGROQ_API_KEY_10=ten\nGROQ_API_KEY_2=two\nOPENROUTER_API_KEY=other\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	client := &apiClient{base: srv.URL, key: "kp_test", http: srv.Client()}
	if err := importEnv(client, []string{path, "--prefix", "GROQ", "--provider", "groq"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "beta", "gamma", "two", "ten"}
	if strings.Join(received, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", received, want)
	}
}
