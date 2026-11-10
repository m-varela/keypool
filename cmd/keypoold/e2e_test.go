package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"keypool/internal/testutil/fakeupstream"
)

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func writeE2EConfig(t *testing.T, dir, address, upstream string) string {
	t.Helper()
	content := fmt.Sprintf(`{
  "listen": %q,
  "default_project": "mathlab",
  "providers": {
    "groq": {
      "type": "openai_compatible",
      "base_url": %q,
      "auth": {"scheme": "bearer"},
      "paths": {"chat": "/chat/completions", "models": "/models"},
      "env_prefix": "GROQ",
      "enabled": true
    }
  },
  "projects": {
    "mathlab": {
      "project": "mathlab",
      "keys": {"groq": {"include": {"tags_any": ["mathlab"]}}},
      "routes": {"prover": {"priority": 1, "chain": ["groq:test-model"]}}
    }
  },
  "warm": {"enabled": false, "l0_enabled": false, "l1_enabled": false}
}`, address, upstream+"/v1")
	path := filepath.Join(dir, "keypool.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitHTTP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("servidor não iniciou em %s", address)
}

func postJSON(t *testing.T, url, token, body string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Keypool-Project", "mathlab")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, data
}

func TestAppBootstrapRestartAndAdminKeyPersistence(t *testing.T) {
	upstream := fakeupstream.New()
	defer upstream.Close()
	const providerKey = "gsk_provider_e2e_secret_0123456789"
	const apiKey = "kp_local_e2e_api_key"
	upstream.Enqueue("Bearer "+providerKey, fakeupstream.JSON(http.StatusOK, `{"id":"chatcmpl-e2e","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))

	dir := t.TempDir()
	address := freeLoopbackAddress(t)
	configPath := writeE2EConfig(t, dir, address, upstream.URL())
	env := map[string]string{"KEYPOOL_DATA_DIR": filepath.Join(dir, "data"), "KEYPOOL_API_KEY": apiKey, "GROQ_API_KEY": providerKey}
	options := appOptions{ConfigPath: configPath, DataDir: env["KEYPOOL_DATA_DIR"], Environ: env}

	first, err := newApp(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, generated := first.GeneratedAPIKey(); generated {
		t.Fatal("API key fornecida pelo ambiente não deveria ser gerada")
	}
	if _, err = newApp(context.Background(), appOptions{DataDir: options.DataDir, Environ: env}); err == nil || !strings.Contains(err.Error(), "porta") {
		t.Fatalf("segunda instância deveria falhar no bind: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- first.Run(ctx) }()
	waitHTTP(t, first.Addr())
	response, body := postJSON(t, "http://"+first.Addr()+"/v1/chat/completions", apiKey, `{"model":"prover","messages":[{"role":"user","content":"teste"}]}`)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("chatcmpl-e2e")) {
		t.Fatalf("E2E status=%d body=%s", response.StatusCode, body)
	}

	adminResponse, adminBody := postJSON(t, "http://"+first.Addr()+"/admin/v1/keys", apiKey, `{"provider":"groq","secret":"gsk_admin_added_e2e_secret","tags":["mathlab"]}`)
	if adminResponse.StatusCode != http.StatusCreated {
		t.Fatalf("admin key status=%d body=%s", adminResponse.StatusCode, adminBody)
	}
	cancel()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon não encerrou")
	}

	wrongEnv := map[string]string{"KEYPOOL_DATA_DIR": options.DataDir, "KEYPOOL_API_KEY": "kp_wrong", "GROQ_API_KEY": providerKey}
	if _, err = newApp(context.Background(), appOptions{DataDir: options.DataDir, Environ: wrongEnv}); err == nil || !strings.Contains(err.Error(), "inválida") {
		t.Fatalf("API key incorreta deveria ser rejeitada: %v", err)
	}

	upstream.Enqueue("Bearer "+providerKey, fakeupstream.JSON(http.StatusOK, `{"id":"chatcmpl-restart","choices":[{"message":{"role":"assistant","content":"restaurado"}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	second, err := newApp(context.Background(), appOptions{DataDir: options.DataDir, Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := second.store.LoadKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundAdminKey := false
	for _, key := range keys {
		if key.Secret.Reveal() == "gsk_admin_added_e2e_secret" {
			foundAdminKey = true
		}
	}
	if !foundAdminKey {
		t.Fatalf("chave criada pela API admin não foi restaurada: %+v", keys)
	}
	pool, err := second.store.LoadPoolState(context.Background(), "groq")
	if err != nil {
		t.Fatal(err)
	}
	if pool.SavedAtMS == 0 {
		t.Fatal("snapshot da janela de limite não foi restaurado")
	}
	daily, err := second.store.ReadProjectDailyTokens(context.Background(), time.Now().UTC())
	if err != nil || daily["mathlab"] < 3 {
		t.Fatalf("contadores diários não foram restaurados: %v err=%v", daily, err)
	}

	ctxSecond, cancelSecond := context.WithCancel(context.Background())
	runSecond := make(chan error, 1)
	go func() { runSecond <- second.Run(ctxSecond) }()
	waitHTTP(t, second.Addr())
	response, body = postJSON(t, "http://"+second.Addr()+"/v1/chat/completions", apiKey, `{"model":"prover","messages":[{"role":"user","content":"após reinício"}]}`)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("chatcmpl-restart")) {
		cancelSecond()
		<-runSecond
		t.Fatalf("E2E após reinício status=%d body=%s", response.StatusCode, body)
	}
	cancelSecond()
	select {
	case runErr := <-runSecond:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon reiniciado não encerrou")
	}

	state, err := os.ReadFile(filepath.Join(options.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{providerKey, apiKey, "gsk_admin_added_e2e_secret"} {
		if bytes.Contains(state, []byte(value)) {
			t.Fatalf("state.json contém segredo em texto claro: %q", value)
		}
	}
}
