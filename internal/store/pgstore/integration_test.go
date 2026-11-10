//go:build integration

package pgstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/domain"
	"keypool/internal/provider"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KEYPOOL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KEYPOOL_TEST_DATABASE_URL nao definido")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	var entropy [8]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "kp_it_" + hex.EncodeToString(entropy[:])
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 4
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPostgresStoreEndToEnd(t *testing.T) {
	pool := integrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migratePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := checkPoolMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := migratePool(ctx, pool); err != nil {
		t.Fatalf("second migration run must be idempotent: %v", err)
	}
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(master)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(pool, box)
	if offset, clockErr := st.DatabaseClockOffset(ctx); clockErr != nil {
		t.Fatalf("DatabaseClockOffset: %v", clockErr)
	} else if offset > time.Minute || offset < -time.Minute {
		t.Fatalf("unexpected database clock offset: %s", offset)
	}
	if err = st.Initialize(ctx, "kp_integration_test_token"); err != nil {
		t.Fatal(err)
	}
	if err = st.VerifyMasterKey(ctx); err != nil {
		t.Fatal(err)
	}
	valid, err := st.VerifyAPIKey(ctx, "kp_integration_test_token")
	if err != nil || !valid {
		t.Fatalf("VerifyAPIKey valid=%v err=%v", valid, err)
	}
	valid, err = st.VerifyAPIKey(ctx, "wrong-token")
	if err != nil || valid {
		t.Fatalf("wrong API key accepted: valid=%v err=%v", valid, err)
	}

	if err = st.RotateAPIKey(ctx, "kp_rotated_integration_token"); err != nil {
		t.Fatalf("RotateAPIKey: %v", err)
	}
	valid, err = st.VerifyAPIKey(ctx, "kp_rotated_integration_token")
	if err != nil || !valid {
		t.Fatalf("rotated API key invalid: valid=%v err=%v", valid, err)
	}
	valid, err = st.VerifyAPIKey(ctx, "kp_integration_test_token")
	if err != nil || valid {
		t.Fatalf("old API key remained valid: valid=%v err=%v", valid, err)
	}

	raw := &config.Raw{Listen: "127.0.0.1:8787", DefaultProject: "mathlab", Providers: map[string]config.ProviderRaw{
		"groq": {Type: "openai_compatible", BaseURL: "https://api.example.test/v1", Auth: provider.AuthSpec{Scheme: "bearer"}, Keys: []sched.KeySpec{{ID: "groq-1", Label: "groq-one", Secret: secret.New("secret-provider-key-123"), Tags: []string{"mathlab"}}}},
	}, Projects: map[string]config.ProjectRaw{
		"mathlab": {Project: "mathlab", Keys: map[string]config.KeyRule{"groq": {Include: config.Selector{TagsAny: []string{"mathlab"}}}}, Routes: map[string]config.RouteRaw{"solve": {Priority: 1, Chain: []string{"groq:model-a"}, Defaults: map[string]any{"temperature": 0.2}, Force: map[string]any{"stream": false}}}},
	}}
	if err = st.SaveConfig(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	if err = pool.QueryRow(ctx, `SELECT secret_enc FROM keys WHERE label='groq-one'`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if len(encrypted) < 1+12+16 || encrypted[0] != cryptobox.Version || bytes.Contains(encrypted, []byte("secret-provider-key-123")) {
		t.Fatal("credential was not stored as a versioned ciphertext")
	}
	loaded, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultProject != "mathlab" || loaded.Listen != raw.Listen {
		t.Fatalf("runtime config mismatch: %+v", loaded)
	}
	keys, err := st.LoadKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Secret.Reveal() != "secret-provider-key-123" {
		t.Fatalf("key round trip mismatch: count=%d", len(keys))
	}
	keys[0].Label = "groq-renamed"
	if err = st.SaveKeys(ctx, "groq", []store.KeyRecord{keys[0]}); err != nil {
		t.Fatalf("SaveKeys: %v", err)
	}
	keys, err = st.LoadKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].Label != "groq-renamed" {
		t.Fatalf("SaveKeys metadata update: %+v err=%v", keys, err)
	}
	loaded.APIKey = "kp_integration_test_token"
	loaded.Providers["groq"] = func() config.ProviderRaw {
		v := loaded.Providers["groq"]
		v.Keys = []sched.KeySpec{{ID: keys[0].ID, Label: keys[0].Label, Secret: keys[0].Secret, Tags: keys[0].Tags}}
		return v
	}()
	if _, err = config.Build(loaded); err != nil {
		t.Fatalf("loaded config does not build: %v", err)
	}
	versionBefore, err := st.ConfigVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bad := &config.Raw{Listen: loaded.Listen, DefaultProject: loaded.DefaultProject, Providers: loaded.Providers, Projects: make(map[string]config.ProjectRaw, len(loaded.Projects))}
	for name, project := range loaded.Projects {
		copyProject := project
		copyProject.Keys = make(map[string]config.KeyRule, len(project.Keys))
		for providerName, rule := range project.Keys {
			copyProject.Keys[providerName] = rule
		}
		copyProject.Routes = make(map[string]config.RouteRaw, len(project.Routes))
		for alias, route := range project.Routes {
			route.Chain = append([]string(nil), route.Chain...)
			route.Chain[0] = "invalid-target-without-provider"
			copyProject.Routes[alias] = route
		}
		bad.Projects[name] = copyProject
	}
	if err = st.SaveConfig(ctx, bad); err == nil {
		t.Fatal("invalid config transaction unexpectedly succeeded")
	}
	versionAfter, err := st.ConfigVersion(ctx)
	if err != nil || versionAfter != versionBefore {
		t.Fatalf("failed config transaction changed version: before=%d after=%d err=%v", versionBefore, versionAfter, err)
	}

	state := sched.PoolSnapshot{SavedAtMS: time.Now().UnixMilli(), Merged: true}
	if err = st.SavePoolState(ctx, "groq", state); err != nil {
		t.Fatal(err)
	}
	gotState, err := st.LoadPoolState(ctx, "groq")
	if err != nil || !gotState.Merged {
		t.Fatalf("snapshot restore: %+v, %v", gotState, err)
	}
	usageEvent := domain.UsageEvent{At: time.Now().UTC().Add(-31 * 24 * time.Hour), RequestID: "req-integration", Project: "mathlab", Alias: "solve", Provider: "groq", Model: "model-a", KeyID: string(keys[0].ID), Outcome: string(domain.Success), Status: 200, Attempt: 1, QueueMS: 7, EstimatedTokens: 12, Usage: &domain.Usage{PromptTokens: 4, CompletionTokens: 8, TotalTokens: 12}}
	usageEvent2 := usageEvent
	usageEvent2.RequestID = "req-integration-2"
	usageEvent2.At = usageEvent.At.Add(time.Second)
	if err = st.AppendUsage(ctx, []domain.UsageEvent{usageEvent, usageEvent2}); err != nil {
		t.Fatal(err)
	}
	nowUTC := time.Now().UTC()
	leaseUsage := domain.UsageEvent{At: nowUTC, RequestID: "lease-integration", Project: "mathlab", Alias: "direct", Provider: "groq", Model: "model-a", KeyID: string(keys[0].ID), Outcome: string(domain.Success), Status: 200, Attempt: 1, Source: "lease", EstimatedTokens: 30, ActualTokens: 25, Usage: &domain.Usage{TotalTokens: 30}}
	passthroughUsage := domain.UsageEvent{At: nowUTC.Add(time.Second), RequestID: "passthrough-integration", Project: "mathlab", Alias: "/v1/search", Provider: "groq", Model: "model-a", KeyID: string(keys[0].ID), Outcome: string(domain.Success), Status: 200, Attempt: 1, Source: "passthrough", EstimatedTokens: 5}
	if err = st.AppendUsage(ctx, []domain.UsageEvent{leaseUsage, passthroughUsage}); err != nil {
		t.Fatal(err)
	}
	dailyTokens, err := st.ReadProjectDailyTokens(ctx, nowUTC.Truncate(24*time.Hour))
	if err != nil || dailyTokens["mathlab"] != 30 {
		t.Fatalf("ReadProjectDailyTokens=%v err=%v, want mathlab=30", dailyTokens, err)
	}
	if err = st.AppendEvent(ctx, sched.Event{Kind: "integration.test", Subject: "mathlab", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	readUsage, err := st.ReadUsage(ctx, time.Now().UTC().Add(-32*24*time.Hour), time.Now().UTC(), "mathlab")
	if err != nil || len(readUsage) != 4 {
		t.Fatalf("ReadUsage rows=%d err=%v", len(readUsage), err)
	}
	readEvents, err := st.ReadEvents(ctx, "integration.test", time.Now().UTC().Add(-time.Hour))
	if err != nil || len(readEvents) != 1 {
		t.Fatalf("ReadEvents rows=%d err=%v", len(readEvents), err)
	}
	var persistedEvents int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE kind='integration.test' AND subject='mathlab'`).Scan(&persistedEvents); err != nil || persistedEvents != 1 {
		t.Fatalf("event persistence count=%d err=%v", persistedEvents, err)
	}

	lock, err := AcquirePoolAdvisoryLock(ctx, pool, DefaultAdvisoryLockKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AcquirePoolAdvisoryLock(ctx, pool, DefaultAdvisoryLockKey); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock error=%v", err)
	}
	if err = lock.Unlock(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err = PrunePool(ctx, pool, time.Now().UTC(), Retention{UsageEvents: 30 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	var requests int
	if err = pool.QueryRow(ctx, `SELECT requests FROM usage_daily WHERE project='mathlab' AND provider='groq' AND model='model-a'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("usage_daily requests=%d, want 2", requests)
	}
	var totalTokens int64
	if err = pool.QueryRow(ctx, `SELECT total_tokens FROM usage_daily WHERE project='mathlab' AND provider='groq' AND model='model-a'`).Scan(&totalTokens); err != nil {
		t.Fatal(err)
	}
	if totalTokens != 24 {
		t.Fatalf("usage_daily total_tokens=%d, want 24", totalTokens)
	}
	if _, err = PrunePool(ctx, pool, time.Now().UTC(), Retention{UsageEvents: 30 * 24 * time.Hour}); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT requests FROM usage_daily WHERE project='mathlab' AND provider='groq' AND model='model-a'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("repeated rollup duplicated usage: requests=%d", requests)
	}
	if err = st.VerifyMasterKey(ctx); err != nil {
		t.Fatal(err)
	}
	wrongBox, _ := cryptobox.New(make([]byte, 32))
	if err = newStore(pool, wrongBox).VerifyMasterKey(ctx); err == nil {
		t.Fatal("wrong master key passed verification")
	}
	if _, err = pool.Exec(ctx, `UPDATE schema_migrations SET checksum=repeat('0',64) WHERE version='0001_init.sql'`); err != nil {
		t.Fatal(err)
	}
	if err = checkPoolMigrations(ctx, pool); err == nil {
		t.Fatal("checksum drift was accepted")
	}
	if _, err = pool.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version='0001_init.sql'`, "7e8b7c666f6f954dd7a782ae198e462c19257273c0acdaa06075c0041f6a515c"); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES('9999_future.sql',repeat('0',64))`); err != nil {
		t.Fatal(err)
	}
	if err = checkPoolMigrations(ctx, pool); err == nil {
		t.Fatal("unknown future migration was accepted")
	}
	if err = st.DeleteKey(ctx, keys[0].ID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	remaining, err := st.LoadKeys(ctx)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("DeleteKey left keys=%d err=%v", len(remaining), err)
	}
}
