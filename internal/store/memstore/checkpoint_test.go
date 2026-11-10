package memstore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/domain"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

func testBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func testRaw() *config.Raw {
	return &config.Raw{
		Listen:         "127.0.0.1:8787",
		DefaultProject: "mathlab",
		Providers: map[string]config.ProviderRaw{
			"groq": {Type: "openai_compatible", BaseURL: "https://example.test/v1", Keys: []sched.KeySpec{{ID: "groq-1", Label: "gsk_…abcd", Secret: secret.New("gsk_provider_secret_never_write_plain")}}},
		},
	}
}

func TestCheckpointRoundTripAndSecretProtection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	st, err := Open(ctx, Options{Dir: dir, Box: testBox(t), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if st.Initialized() {
		t.Fatal("store novo não deveria estar inicializado")
	}
	if _, err = os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("Open criou state.json: %v", err)
	}
	const token = "kp_api_key_secret_never_write_plain"
	if err = st.Initialize(ctx, token, testRaw()); err != nil {
		t.Fatal(err)
	}
	pool := sched.PoolSnapshot{SavedAtMS: 1234, Merged: true}
	if err = st.SavePoolState(ctx, "groq", pool); err != nil {
		t.Fatal(err)
	}
	usage := domain.UsageEvent{At: clk.Now(), Project: "mathlab", Provider: "groq", KeyID: "groq-1", Source: "request", ActualTokens: 23}
	if err = st.AppendUsage(ctx, []domain.UsageEvent{usage}); err != nil {
		t.Fatal(err)
	}
	if err = st.SavePoolState(ctx, "groq", pool); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"gsk_provider_secret_never_write_plain", token} {
		if strings.Contains(string(data), value) {
			t.Fatalf("state.json contém segredo em texto claro: %q", value)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("permissão state.json=%04o, esperado 0600", info.Mode().Perm())
	}

	reopened, err := Open(ctx, Options{Dir: dir, Box: testBox(t), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := reopened.VerifyAPIKey(ctx, token)
	if err != nil || !valid {
		t.Fatalf("VerifyAPIKey valid=%v err=%v", valid, err)
	}
	valid, err = reopened.VerifyAPIKey(ctx, "kp_wrong")
	if err != nil || valid {
		t.Fatalf("API key incorreta aceita: valid=%v err=%v", valid, err)
	}
	raw, err := reopened.LoadConfig(ctx)
	if err != nil || raw.Listen != "127.0.0.1:8787" {
		t.Fatalf("LoadConfig=%+v err=%v", raw, err)
	}
	keys, err := reopened.LoadKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Secret.Reveal() != "gsk_provider_secret_never_write_plain" {
		t.Fatalf("LoadKeys=%v err=%v", keys, err)
	}
	got, err := reopened.LoadPoolState(ctx, "groq")
	if err != nil || got.SavedAtMS != 1234 || !got.Merged {
		t.Fatalf("LoadPoolState=%+v err=%v", got, err)
	}
	daily, err := reopened.ReadProjectDailyTokens(ctx, clk.Now())
	if err != nil || daily["mathlab"] != 23 {
		t.Fatalf("daily=%v err=%v", daily, err)
	}
}

func TestCheckpointRejectsWrongMasterKeyCorruptionAndVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(ctx, Options{Dir: dir, Box: testBox(t), Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Initialize(ctx, "kp_test", testRaw()); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	wrongBox, err := cryptobox.New([]byte("abcdef0123456789abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, Options{Dir: dir, Box: wrongBox}); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("master key errada deveria falhar claramente: %v", err)
	}
	path := filepath.Join(dir, "state.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, Options{Dir: dir, Box: testBox(t)}); err == nil {
		t.Fatal("JSON corrompido foi aceito")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(original), `"version": 1`, `"version": 999`, 1)
	if err = os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, Options{Dir: dir, Box: testBox(t)}); err == nil || !strings.Contains(err.Error(), "versão") {
		t.Fatalf("versão desconhecida deveria falhar: %v", err)
	}
}

func TestFailedCheckpointDoesNotChangeMemory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "state.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, Options{Dir: dir, Box: testBox(t), Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Initialize(ctx, "kp_test", testRaw()); err == nil {
		t.Fatal("Initialize deveria falhar ao gravar tmp")
	}
	if st.Initialized() {
		t.Fatal("falha de gravação alterou o estado em memória")
	}
}

func TestDailyCountersRollAndWarmupNormalization(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC))
	st := New(nil)
	st.clock = clk
	events := []domain.UsageEvent{
		{At: clk.Now(), Project: "mathlab", Source: "warmup", ActualTokens: 99},
		{At: clk.Now(), Project: "mathlab", Source: "other", EstimatedTokens: 10},
		{At: clk.Now(), Project: "mathlab", Source: "lease", Usage: &domain.Usage{TotalTokens: 5}},
		{At: clk.Now(), Project: "mathlab", Provider: "groq", KeyID: "groq-1", Source: "warmup", Alias: "warmup-L2", EstimatedTokens: 7},
	}
	if err := st.AppendUsage(ctx, events); err != nil {
		t.Fatal(err)
	}
	daily, err := st.ReadProjectDailyTokens(ctx, clk.Now())
	if err != nil || daily["mathlab"] != 15 {
		t.Fatalf("daily=%v err=%v", daily, err)
	}
	warm, err := st.CountWarmL2Tokens(ctx, "groq", "groq-1", clk.Now().Truncate(24*time.Hour), clk.Now().Add(time.Minute))
	if err != nil || warm != 7 {
		t.Fatalf("warm L2=%d err=%v", warm, err)
	}
	clk.Advance(2 * time.Minute)
	if err = st.AppendUsage(ctx, []domain.UsageEvent{{At: clk.Now(), Project: "mathlab", Source: "request", EstimatedTokens: 3}}); err != nil {
		t.Fatal(err)
	}
	daily, err = st.ReadProjectDailyTokens(ctx, clk.Now())
	if err != nil || daily["mathlab"] != 3 {
		t.Fatalf("daily após virada UTC=%v err=%v", daily, err)
	}
}

func TestUsageRingRetentionAndMemoryFootprint(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	batch := make([]domain.UsageEvent, usageCapacity)
	for i := range batch {
		batch[i] = domain.UsageEvent{At: clk.Now(), RequestID: "request-id", Project: "mathlab", Provider: "groq", Model: "model", KeyID: "key", EstimatedTokens: 128}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	st := New(nil)
	st.clock = clk
	if err := st.AppendUsage(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	used := after.Alloc - before.Alloc
	t.Logf("memória alocada para %d eventos: %.2f MiB", usageCapacity, float64(used)/(1024*1024))
	if len(st.usage.values()) != usageCapacity {
		t.Fatalf("ring contém %d eventos", len(st.usage.values()))
	}
	clk.Advance(usageRetention + time.Second)
	if err := st.AppendUsage(context.Background(), []domain.UsageEvent{{At: clk.Now(), Project: "mathlab"}}); err != nil {
		t.Fatal(err)
	}
	if got := len(st.usage.values()); got != 1 {
		t.Fatalf("após retenção o ring contém %d eventos", got)
	}
	if got := st.UsageEvicted(); got != usageCapacity {
		t.Fatalf("UsageEvicted=%d, esperado %d", got, usageCapacity)
	}
}

func TestConcurrentDurableAndUsageOperations(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	st, err := Open(ctx, Options{Dir: t.TempDir(), Box: testBox(t), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Initialize(ctx, "kp_test", testRaw()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = st.AppendUsage(ctx, []domain.UsageEvent{{At: clk.Now(), Project: "mathlab", EstimatedTokens: 1}})
				_ = st.SavePoolState(ctx, "groq", sched.PoolSnapshot{SavedAtMS: int64(i*100 + j)})
				_, _ = st.ReadUsage(ctx, time.Time{}, time.Time{}, "")
				_, _ = st.LoadKeys(ctx)
			}
		}(i)
	}
	wg.Wait()
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
}
