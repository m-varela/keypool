package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"keypool/internal/admin"
	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/domain"
	"keypool/internal/gateway"
	keymetrics "keypool/internal/metrics"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/store"
	"keypool/internal/store/memstore"
	"keypool/internal/warm"
)

type appOptions struct {
	ConfigPath     string
	DataDir        string
	MasterKeyPath  string
	MasterKeyDPAPI bool
	Environ        map[string]string
	Clock          clock.Clock
}

type app struct {
	store      *memstore.Store
	listener   net.Listener
	snapshot   *config.Snapshot
	router     *router.Router
	apiKey     string
	generated  bool
	clock      clock.Clock
	runStarted bool
}

// newApp prepara o estado e faz o bind antes da primeira gravação durável.
func newApp(ctx context.Context, options appOptions) (*app, error) {
	if options.Clock == nil {
		options.Clock = clock.Real()
	}
	if options.Environ == nil {
		options.Environ = environment()
	}
	if options.DataDir == "" {
		options.DataDir = dataDirFor(options.Environ)
	}
	if options.MasterKeyPath == "" {
		options.MasterKeyPath = masterKeyPathFor(options.Environ, options.DataDir)
	}

	master, saveMaster, err := resolveMasterKey(options.MasterKeyPath, options.Environ["KEYPOOL_MASTER_KEY"], options.MasterKeyDPAPI)
	if err != nil {
		return nil, err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return nil, err
	}
	st, err := memstore.Open(ctx, memstore.Options{Dir: options.DataDir, Box: box, Clock: options.Clock})
	if err != nil {
		return nil, err
	}
	cleanup := func(listener net.Listener) {
		if listener != nil {
			_ = listener.Close()
		}
		_ = st.Close()
	}

	initialized := st.Initialized()
	apiKey := strings.TrimSpace(options.Environ["KEYPOOL_API_KEY"])
	generated := false
	var raw *config.Raw
	if initialized {
		if apiKey == "" {
			cleanup(nil)
			return nil, errors.New("KEYPOOL_API_KEY ausente; defina a chave usada na primeira execução")
		}
		valid, verifyErr := st.VerifyAPIKey(ctx, apiKey)
		if verifyErr != nil || !valid {
			cleanup(nil)
			if verifyErr != nil {
				return nil, verifyErr
			}
			return nil, errors.New("KEYPOOL_API_KEY inválida")
		}
		raw, err = st.LoadConfig(ctx)
		if err != nil {
			cleanup(nil)
			return nil, err
		}
		if options.ConfigPath != "" {
			log.Printf("--config ignorado: o estado já possui configuração; use a API administrativa")
		}
	} else {
		if options.ConfigPath == "" {
			cleanup(nil)
			return nil, errors.New("primeira execução exige --config <arquivo.json>")
		}
		raw, err = config.LoadFile(options.ConfigPath, options.Environ)
		if err != nil {
			cleanup(nil)
			return nil, fmt.Errorf("carregar configuração inicial: %w", err)
		}
		if apiKey == "" {
			apiKey, err = newAPIKey()
			if err != nil {
				cleanup(nil)
				return nil, err
			}
			generated = true
		}
	}
	raw.APIKey = apiKey
	if initialized {
		keyRecords, loadErr := st.LoadKeys(ctx)
		if loadErr != nil {
			cleanup(nil)
			return nil, loadErr
		}
		for name, provider := range raw.Providers {
			provider.Keys = nil
			for _, record := range keyRecords {
				if record.Provider == name && record.Enabled {
					provider.Keys = append(provider.Keys, sched.KeySpec{
						ID: record.ID, Label: record.Label, Secret: record.Secret,
						Group: record.Group, Tags: append([]string(nil), record.Tags...),
					})
				}
			}
			raw.Providers[name] = provider
		}
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		cleanup(nil)
		return nil, err
	}
	rt, err := router.New(snapshot)
	if err != nil {
		cleanup(nil)
		return nil, err
	}
	addr := snapshot.Listen()
	if !isLoopbackAddress(addr) {
		cleanup(nil)
		return nil, fmt.Errorf("listen deve ser loopback; recebido: %s", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		cleanup(nil)
		return nil, fmt.Errorf("porta %s ocupada ou outra instância em execução: %w", addr, err)
	}
	// Nenhum arquivo é criado antes de obter o listener exclusivo.
	if saveMaster != nil {
		if err = saveMaster(); err != nil {
			cleanup(listener)
			return nil, fmt.Errorf("gravar master key: %w", err)
		}
	}
	if !initialized {
		if err = st.Initialize(ctx, apiKey, raw); err != nil {
			cleanup(listener)
			return nil, fmt.Errorf("inicializar state.json: %w", err)
		}
		log.Printf("configuração inicial importada de %s", options.ConfigPath)
	}
	return &app{store: st, listener: listener, snapshot: snapshot, router: rt, apiKey: apiKey, generated: generated, clock: options.Clock}, nil
}

func (a *app) Addr() string {
	if a == nil || a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

func (a *app) GeneratedAPIKey() (string, bool) {
	if a == nil || !a.generated {
		return "", false
	}
	return a.apiKey, true
}

func (a *app) Run(ctx context.Context) error {
	if a == nil || a.store == nil || a.listener == nil {
		return errors.New("aplicação não inicializada")
	}
	if a.runStarted {
		return errors.New("aplicação já foi iniciada")
	}
	a.runStarted = true
	if err := restoreDailyProjectUsage(ctx, a.store, a.router, a.clock); err != nil {
		_ = a.listener.Close()
		_ = a.store.Close()
		return err
	}
	snapshotter := store.NewDynamicSnapshotter(a.store, func() map[string]store.SnapshotPool {
		return snapshotPools(a.router)
	}, 10*time.Second, func(err error) { log.Printf("persistir snapshot: %v", err) })
	if err := snapshotter.Restore(ctx); err != nil {
		_ = a.listener.Close()
		snapshotter.Close()
		_ = a.store.Close()
		return fmt.Errorf("restaurar estado dos limites: %w", err)
	}
	snapshotter.Start()
	a.router.SetOnStateChange(snapshotter.MarkDirty)

	gw := gateway.New(a.snapshot, a.router)
	metrics := keymetrics.New()
	wm := warm.New(a.snapshot.WarmConfig(), a.router, a.store)
	gw.SetEventSink(func(event sched.Event) {
		if err := a.store.AppendEvent(context.Background(), event); err != nil {
			log.Printf("registrar evento do gateway: %v", err)
		}
	})
	wm.SetUsageSink(func(event domain.UsageEvent) {
		if err := a.store.AppendUsage(context.Background(), []domain.UsageEvent{event}); err != nil {
			log.Printf("registrar uso de warm: %v", err)
		}
	})
	wm.SetEventSink(func(event sched.Event) {
		if err := a.store.AppendEvent(context.Background(), event); err != nil {
			log.Printf("registrar evento de warm: %v", err)
		}
	})
	gw.SetUsageSink(func(event domain.UsageEvent) {
		if err := a.store.AppendUsage(context.Background(), []domain.UsageEvent{event}); err != nil {
			log.Printf("registrar uso: %v", err)
		}
	})
	gw.SetMetrics(metrics, func(registry *keymetrics.Registry) {
		collectRuntimeMetrics(registry, a.router, wm, a.store.UsageEvicted)
	})
	gw.SetReadiness(false, "aguardando primeira passada L1")
	wm.SetOnReady(gw.SetReadiness)
	publish := func(next *config.Snapshot) error {
		if err := gw.SetSnapshot(next); err != nil {
			return err
		}
		a.router.MarkStateDirty()
		wm.SetConfig(next.WarmConfig())
		return nil
	}
	adminAPI, err := admin.New(a.store, a.apiKey, publish)
	if err != nil {
		_ = a.listener.Close()
		wm.Close()
		gw.CloseLeases()
		snapshotter.Close()
		_ = a.store.Close()
		return err
	}
	adminAPI.SetWarmer(wm)
	gw.SetAdminHandler(adminAPI.Handler())
	a.router.SetOnEvent(func(providerName string, event sched.Event) {
		if event.Subject == "" {
			event.Subject = providerName
		} else {
			event.Subject = providerName + "/" + event.Subject
		}
		if err := a.store.AppendEvent(context.Background(), event); err != nil {
			log.Printf("registrar evento do scheduler: %v", err)
		}
	})
	if err = wm.Start(ctx); err != nil {
		_ = a.listener.Close()
		wm.Close()
		gw.CloseLeases()
		snapshotter.Close()
		_ = a.store.Close()
		return err
	}

	srv := &http.Server{Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("KeyPool ouvindo em http://%s", a.Addr())
		serveErr <- srv.Serve(a.listener)
	}()
	var runErr error
	select {
	case err = <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err = srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		if runErr == nil {
			runErr = err
		}
	}
	cancel()
	wm.Close()
	gw.CloseLeases()
	snapshotter.Close()
	if err = a.store.Close(); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func restoreDailyProjectUsage(ctx context.Context, st store.Store, rt *router.Router, clk clock.Clock) error {
	reader, ok := st.(store.ProjectDailyUsageReader)
	if !ok {
		return errors.New("store não implementa leitura do orçamento diário por projeto")
	}
	from := clk.Now().UTC().Truncate(24 * time.Hour)
	loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	totals, err := reader.ReadProjectDailyTokens(loadCtx, from)
	if err != nil {
		return fmt.Errorf("restaurar orçamento diário dos projetos: %w", err)
	}
	rt.SetProjectDailyUsage(totals)
	return nil
}

func snapshotPools(rt *router.Router) map[string]store.SnapshotPool {
	out := make(map[string]store.SnapshotPool)
	if rt == nil {
		return out
	}
	for name, pool := range rt.Pools() {
		out[name] = pool
	}
	return out
}

func resolveMasterKey(path, env string, useDPAPI bool) ([]byte, func() error, error) {
	if strings.TrimSpace(env) != "" {
		if useDPAPI {
			return nil, nil, errors.New("--master-key-dpapi não pode ser combinado com KEYPOOL_MASTER_KEY")
		}
		key, err := cryptobox.MasterKeyFromEnv(env)
		return key, nil, err
	}
	if _, err := os.Stat(path); err == nil {
		if useDPAPI {
			key, readErr := cryptobox.LoadDPAPIMasterKey(path)
			return key, nil, readErr
		}
		key, readErr := cryptobox.LoadOrCreateMasterKey(path, "")
		return key, nil, readErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, err
	}
	persist := func() error {
		if useDPAPI {
			return cryptobox.SaveDPAPIMasterKey(path, key)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err = file.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return err
		}
		if err = file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return err
		}
		return file.Close()
	}
	return key, persist, nil
}

func dataDirFor(env map[string]string) string {
	if path := env["KEYPOOL_DATA_DIR"]; path != "" {
		return path
	}
	if path := env["LOCALAPPDATA"]; path != "" {
		return filepath.Join(path, "keypool")
	}
	if path := env["XDG_CONFIG_HOME"]; path != "" {
		return filepath.Join(path, "keypool")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".keypool"
	}
	return filepath.Join(home, ".keypool")
}

func masterKeyPathFor(env map[string]string, dir string) string {
	if path := env["KEYPOOL_MASTER_KEY_FILE"]; path != "" {
		return path
	}
	return filepath.Join(dir, "master.key")
}

func environment() map[string]string {
	out := map[string]string{}
	for _, item := range os.Environ() {
		pair := strings.SplitN(item, "=", 2)
		if len(pair) == 2 {
			out[pair[0]] = pair[1]
		}
	}
	return out
}

func newAPIKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "kp_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}

func dataDir() string       { return dataDirFor(environment()) }
func masterKeyPath() string { return masterKeyPathFor(environment(), dataDir()) }

func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readOnlyMasterKey(path, env string, useDPAPI bool) ([]byte, error) {
	if strings.TrimSpace(env) != "" {
		if useDPAPI {
			return nil, errors.New("--master-key-dpapi não pode ser combinado com KEYPOOL_MASTER_KEY")
		}
		return cryptobox.MasterKeyFromEnv(env)
	}
	if useDPAPI {
		return cryptobox.LoadDPAPIMasterKey(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return cryptobox.MasterKeyFromEnv(string(data))
}
