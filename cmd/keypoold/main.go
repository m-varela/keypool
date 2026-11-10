// keypoold inicia e administra o gateway local KeyPool.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"keypool/internal/admin"
	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/domain"
	eventwriter "keypool/internal/events"
	"keypool/internal/gateway"
	keymetrics "keypool/internal/metrics"
	"keypool/internal/router"
	"keypool/internal/sched"
	"keypool/internal/store"
	"keypool/internal/store/memstore"
	"keypool/internal/store/pgstore"
	usagewriter "keypool/internal/usage"
	"keypool/internal/warm"
)

const version = "0.4.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "init":
		err = runInit(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "version":
		fmt.Printf("keypoold %s (%s)\n", version, runtime.Version())
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Print(err)
		os.Exit(1)
	}
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dsn := fs.String("database", os.Getenv("KEYPOOL_DATABASE_URL"), "URL PostgreSQL (ou KEYPOOL_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dsn) == "" {
		return errors.New("migrate exige KEYPOOL_DATABASE_URL ou --database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pgstore.Migrate(ctx, *dsn); err != nil {
		return err
	}
	fmt.Println("Migrações KeyPool aplicadas e checksums verificados.")
	return nil
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dsn := fs.String("database", os.Getenv("KEYPOOL_DATABASE_URL"), "URL PostgreSQL")
	keyPath := fs.String("master-key-file", masterKeyPath(), "arquivo da master key")
	useDPAPI := fs.Bool("master-key-dpapi", false, "proteger a master key com DPAPI no Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dsn) == "" {
		return errors.New("init exige KEYPOOL_DATABASE_URL ou --database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var master []byte
	var err error
	if *useDPAPI {
		if strings.TrimSpace(os.Getenv("KEYPOOL_MASTER_KEY")) != "" {
			return errors.New("--master-key-dpapi nao pode ser combinado com KEYPOOL_MASTER_KEY")
		}
		master, err = cryptobox.LoadOrCreateDPAPIMasterKey(*keyPath)
	} else {
		master, err = cryptobox.LoadOrCreateMasterKey(*keyPath, os.Getenv("KEYPOOL_MASTER_KEY"))
	}
	if err != nil {
		return err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return err
	}
	st, err := pgstore.Open(ctx, *dsn, box)
	if err != nil {
		return err
	}
	defer st.Close()
	if err = st.CheckMigrations(ctx); err != nil {
		return fmt.Errorf("execute keypoold migrate antes de init: %w", err)
	}
	token, err := newAPIKey()
	if err != nil {
		return err
	}
	if err = st.Initialize(ctx, token); err != nil {
		return err
	}
	fmt.Println("KeyPool inicializado.")
	fmt.Println("API key (exibida uma unica vez; salve-a em KEYPOOL_API_KEY):")
	fmt.Println(token)
	if *useDPAPI {
		fmt.Printf("Master key protegida pelo DPAPI do usuario atual: %s\n", *keyPath)
	} else if strings.TrimSpace(os.Getenv("KEYPOOL_MASTER_KEY")) != "" {
		fmt.Println("Master key: fornecida por KEYPOOL_MASTER_KEY (nenhum arquivo foi criado)")
	} else {
		fmt.Printf("Master key: %s\n", *keyPath)
	}
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

func restoreDailyProjectUsage(ctx context.Context, st store.Store, rt *router.Router) error {
	reader, ok := st.(store.ProjectDailyUsageReader)
	if !ok {
		return errors.New("store não implementa leitura do orçamento diário por projeto")
	}
	from := time.Now().UTC().Truncate(24 * time.Hour)
	loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	totals, err := reader.ReadProjectDailyTokens(loadCtx, from)
	if err != nil {
		return fmt.Errorf("restaurar orçamento diário dos projetos: %w", err)
	}
	rt.SetProjectDailyUsage(totals)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dev := fs.String("dev", "", "arquivo YAML para modo de desenvolvimento em memoria")
	configPath := fs.String("config", "", "arquivo YAML importado apenas quando o banco ainda nao possui configuracao")
	dsn := fs.String("database", os.Getenv("KEYPOOL_DATABASE_URL"), "URL PostgreSQL")
	keyPath := fs.String("master-key-file", masterKeyPath(), "arquivo da master key")
	useDPAPI := fs.Bool("master-key-dpapi", false, "ler master key protegida pelo DPAPI no Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dev != "" {
		if *useDPAPI {
			return errors.New("--master-key-dpapi aplica somente ao modo persistente PostgreSQL, nao a --dev")
		}
		return serveDev(*dev)
	}
	if strings.TrimSpace(*dsn) == "" {
		return errors.New("serve exige KEYPOOL_DATABASE_URL ou --database")
	}
	if strings.TrimSpace(os.Getenv("KEYPOOL_API_KEY")) == "" {
		return errors.New("KEYPOOL_API_KEY ausente; use a chave exibida por keypoold init")
	}

	var master []byte
	var err error
	if *useDPAPI {
		if strings.TrimSpace(os.Getenv("KEYPOOL_MASTER_KEY")) != "" {
			return errors.New("--master-key-dpapi nao pode ser combinado com KEYPOOL_MASTER_KEY")
		}
		master, err = cryptobox.LoadDPAPIMasterKey(*keyPath)
	} else {
		master, err = loadExistingMasterKey(*keyPath, os.Getenv("KEYPOOL_MASTER_KEY"))
	}
	if err != nil {
		return err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	openCtx, openCancel := context.WithTimeout(ctx, 20*time.Second)
	st, err := pgstore.Open(openCtx, *dsn, box)
	openCancel()
	if err != nil {
		return err
	}
	defer st.Close()
	if err = st.CheckMigrations(ctx); err != nil {
		return fmt.Errorf("migracoes pendentes/inconsistentes; execute keypoold migrate: %w", err)
	}
	if err = st.VerifyMasterKey(ctx); err != nil {
		return fmt.Errorf("validacao da master key: %w", err)
	}
	valid, err := st.VerifyAPIKey(ctx, os.Getenv("KEYPOOL_API_KEY"))
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("KEYPOOL_API_KEY invalida")
	}
	lock, err := st.AcquireAdvisoryLock(ctx, pgstore.DefaultAdvisoryLockKey)
	if err != nil {
		return err
	}
	defer func() {
		unlockCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		if e := lock.Unlock(unlockCtx); e != nil {
			log.Printf("liberar trava PostgreSQL: %v", e)
		}
	}()

	raw, err := st.LoadConfig(ctx)
	if errors.Is(err, pgstore.ErrNoConfig) {
		if *configPath == "" {
			return fmt.Errorf("banco sem configuracao; inicialize com keypoold serve --config <arquivo.yaml>")
		}
		env := environment()
		raw, err = config.LoadDev(*configPath, env)
		if err != nil {
			return fmt.Errorf("carregar configuracao inicial: %w", err)
		}
		raw.APIKey = os.Getenv("KEYPOOL_API_KEY")
		if _, err = config.Build(raw); err != nil {
			return fmt.Errorf("validar configuracao inicial: %w", err)
		}
		if err = st.SaveConfig(ctx, raw); err != nil {
			return err
		}
		log.Printf("configuracao inicial importada do arquivo %s", *configPath)
	} else if err != nil {
		return err
	} else if *configPath != "" {
		log.Printf("--config ignorado: o banco ja possui configuracao; use a API administrativa")
	}
	raw.APIKey = os.Getenv("KEYPOOL_API_KEY")
	keyRecords, err := st.LoadKeys(ctx)
	if err != nil {
		return err
	}
	for name, pr := range raw.Providers {
		pr.Keys = nil
		for _, rec := range keyRecords {
			if rec.Provider == name && rec.Enabled {
				pr.Keys = append(pr.Keys, sched.KeySpec{ID: rec.ID, Label: rec.Label, Secret: rec.Secret, Group: rec.Group, Tags: rec.Tags})
			}
		}
		raw.Providers[name] = pr
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		return err
	}
	rt, err := router.New(snapshot)
	if err != nil {
		return err
	}
	if err := restoreDailyProjectUsage(ctx, st, rt); err != nil {
		return err
	}
	eventLog := eventwriter.NewWriter(st, eventwriter.Options{QueueSize: 512, Timeout: 5 * time.Second, OnError: func(e error) { log.Printf("persistir evento: %v", e) }})
	defer eventLog.Close()
	rt.SetOnEvent(func(providerName string, event sched.Event) {
		if event.Subject == "" {
			event.Subject = providerName
		} else {
			event.Subject = providerName + "/" + event.Subject
		}
		eventLog.Submit(event)
	})
	snapshotter := store.NewDynamicSnapshotter(st, func() map[string]store.SnapshotPool {
		return snapshotPools(rt)
	}, 10*time.Second, func(e error) { log.Printf("persistir snapshot: %v", e) })
	if err = snapshotter.Restore(ctx); err != nil {
		return fmt.Errorf("restaurar estado dos limites: %w", err)
	}
	snapshotter.Start()
	defer snapshotter.Close()
	rt.SetOnStateChange(snapshotter.MarkDirty)
	writer := usagewriter.NewWriter(st, usagewriter.Options{QueueSize: 8192, BatchSize: 200, Interval: time.Second, Timeout: 5 * time.Second, OnError: func(e error) { log.Printf("persistir uso: %v", e) }})
	defer writer.Close()
	gw := gateway.New(snapshot, rt)
	metricRegistry := keymetrics.New()
	defer gw.CloseLeases()
	gw.SetEventSink(func(e sched.Event) { eventLog.Submit(e) })
	wm := warm.New(snapshot.WarmConfig(), rt, st)
	wm.SetUsageSink(func(e domain.UsageEvent) { writer.Submit(e) })
	wm.SetEventSink(func(e sched.Event) { eventLog.Submit(e) })
	gw.SetMetrics(metricRegistry, func(reg *keymetrics.Registry) { collectRuntimeMetrics(reg, rt, wm, writer) })
	gw.SetReadiness(false, "aguardando primeira passada L1")
	wm.SetOnReady(gw.SetReadiness)
	publish := func(next *config.Snapshot) error {
		if err := gw.SetSnapshot(next); err != nil {
			return err
		}
		rt.MarkStateDirty()
		wm.SetConfig(next.WarmConfig())
		return nil
	}
	adminAPI, adminErr := admin.New(st, os.Getenv("KEYPOOL_API_KEY"), publish)
	if adminErr != nil {
		return adminErr
	}
	adminAPI.SetWarmer(wm)
	gw.SetAdminHandler(adminAPI.Handler())
	// The callback stays non-blocking; database work belongs to usage.Writer.
	gw.SetUsageSink(func(e domain.UsageEvent) { writer.Submit(e) })
	if err := wm.Start(ctx); err != nil {
		return err
	}
	defer wm.Close()
	addr := snapshot.Listen()
	if !isLoopbackAddress(addr) {
		return fmt.Errorf("listen deve ser loopback; recebido: %s", addr)
	}
	srv := &http.Server{Addr: addr, Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	retentionCtx, stopRetention := context.WithCancel(ctx)
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		retentionLoop(retentionCtx, st)
	}()
	serveErr := make(chan error, 1)
	go func() { log.Printf("KeyPool PostgreSQL ouvindo em http://%s", addr); serveErr <- srv.ListenAndServe() }()
	var runErr error
	select {
	case e := <-serveErr:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			runErr = e
		}
	case <-ctx.Done():
	}
	stopRetention()
	<-retentionDone
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if e := srv.Shutdown(shutdownCtx); e != nil {
		_ = srv.Close()
		if runErr == nil {
			runErr = e
		}
	}
	shutdownCancel()
	// Defers flush the snapshotter and usage writer after HTTP traffic has stopped.
	return runErr
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dsn := fs.String("database", os.Getenv("KEYPOOL_DATABASE_URL"), "URL PostgreSQL")
	keyPath := fs.String("master-key-file", masterKeyPath(), "arquivo da master key")
	useDPAPI := fs.Bool("master-key-dpapi", false, "ler master key protegida pelo DPAPI no Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dsn) == "" {
		return errors.New("doctor exige KEYPOOL_DATABASE_URL ou --database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var master []byte
	var err error
	if *useDPAPI {
		if strings.TrimSpace(os.Getenv("KEYPOOL_MASTER_KEY")) != "" {
			return errors.New("--master-key-dpapi nao pode ser combinado com KEYPOOL_MASTER_KEY")
		}
		master, err = cryptobox.LoadDPAPIMasterKey(*keyPath)
	} else {
		master, err = loadExistingMasterKey(*keyPath, os.Getenv("KEYPOOL_MASTER_KEY"))
	}
	if err != nil {
		return err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return err
	}
	st, err := pgstore.Open(ctx, *dsn, box)
	if err != nil {
		return fmt.Errorf("PostgreSQL: %w", err)
	}
	defer st.Close()
	fmt.Println("[OK] PostgreSQL acessivel")
	clockOffset, err := st.DatabaseClockOffset(ctx)
	if err != nil {
		return err
	}
	if clockOffset > 5*time.Second || clockOffset < -5*time.Second {
		fmt.Printf("[AVISO] Relogio PostgreSQL/local diverge em %s; sincronize o horario do sistema\n", clockOffset.Round(time.Millisecond))
	} else {
		fmt.Printf("[OK] Relogio PostgreSQL/local (desvio estimado %s)\n", clockOffset.Round(time.Millisecond))
	}
	if err = st.CheckMigrations(ctx); err != nil {
		return err
	}
	fmt.Println("[OK] Migracoes e checksums")
	if err = st.VerifyMasterKey(ctx); err != nil {
		return err
	}
	fmt.Println("[OK] Master key corresponde ao banco")
	prefix, created, rotated, err := st.APIKeyInfo(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("[OK] API key configurada (%s; criada %s)", prefix, created.Format(time.RFC3339))
	if !rotated.IsZero() {
		fmt.Printf("; rotacionada %s", rotated.Format(time.RFC3339))
	}
	fmt.Println()
	if token := os.Getenv("KEYPOOL_API_KEY"); token != "" {
		ok, e := st.VerifyAPIKey(ctx, token)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("KEYPOOL_API_KEY do ambiente nao corresponde ao hash persistido")
		}
		fmt.Println("[OK] KEYPOOL_API_KEY do ambiente")
	} else {
		fmt.Println("[AVISO] KEYPOOL_API_KEY nao definida no ambiente")
	}
	if *useDPAPI {
		fmt.Println("[OK] Master key protegida pelo DPAPI do usuario atual")
	} else if strings.TrimSpace(os.Getenv("KEYPOOL_MASTER_KEY")) != "" {
		fmt.Println("[OK] Master key fornecida por KEYPOOL_MASTER_KEY")
	} else if runtime.GOOS != "windows" {
		info, e := os.Stat(*keyPath)
		if e != nil {
			return fmt.Errorf("arquivo master key: %w", e)
		}
		if info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("permissoes inseguras em %s: %04o (esperado 0600)", *keyPath, info.Mode().Perm())
		}
		fmt.Println("[OK] Permissoes Unix da master key")
	} else {
		if _, e := os.Stat(*keyPath); e != nil {
			return fmt.Errorf("arquivo master key: %w", e)
		}
		fmt.Println("[INFO] ACL Windows: execute scripts/windows/acl-master-key.ps1")
	}
	fmt.Printf("[INFO] Runtime Go: %s\n", runtime.Version())
	if raw, e := st.LoadConfig(ctx); e == nil {
		addr := raw.Listen
		if addr == "" {
			addr = "127.0.0.1:8787"
		}
		if !isLoopbackAddress(addr) {
			return fmt.Errorf("listen nao e loopback: %s", addr)
		}
		ln, e := net.Listen("tcp", addr)
		if e != nil {
			return fmt.Errorf("porta %s indisponivel: %w", addr, e)
		}
		_ = ln.Close()
		fmt.Printf("[OK] Endereco %s disponivel\n", addr)
	} else if !errors.Is(e, pgstore.ErrNoConfig) {
		return e
	} else {
		fmt.Println("[INFO] Nenhuma configuracao de providers/projetos aplicada")
	}
	fmt.Println("Doctor finalizado.")
	return nil
}

func serveDev(path string) error {
	env := environment()
	generated := false
	if strings.TrimSpace(env["KEYPOOL_API_KEY"]) == "" {
		token, e := newAPIKey()
		if e != nil {
			return e
		}
		env["KEYPOOL_API_KEY"] = token
		generated = true
	}
	raw, e := config.LoadDev(path, env)
	if e != nil {
		return e
	}
	if raw.APIKey == "" {
		raw.APIKey = env["KEYPOOL_API_KEY"]
	}
	devStore := memstore.New(raw)
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 5*time.Second)
	storedRaw, e := devStore.LoadConfig(loadCtx)
	cancelLoad()
	if e != nil {
		return fmt.Errorf("carregar configuracao do memstore: %w", e)
	}
	snap, e := config.Build(storedRaw)
	if e != nil {
		return e
	}
	rt, e := router.New(snap)
	if e != nil {
		return e
	}
	if e := restoreDailyProjectUsage(context.Background(), devStore, rt); e != nil {
		return e
	}
	snapshotter := store.NewDynamicSnapshotter(devStore, func() map[string]store.SnapshotPool {
		return snapshotPools(rt)
	}, 10*time.Second, func(err error) {
		log.Printf("snapshot dev: %v", err)
	})
	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 5*time.Second)
	if e = snapshotter.Restore(restoreCtx); e != nil {
		cancelRestore()
		return fmt.Errorf("restaurar estado de pools em memoria: %w", e)
	}
	cancelRestore()
	snapshotter.Start()
	defer snapshotter.Close()
	rt.SetOnStateChange(snapshotter.MarkDirty)
	rt.SetOnEvent(func(providerName string, event sched.Event) {
		if event.Detail == "" {
			event.Detail = "provider=" + providerName
		} else {
			event.Detail = "provider=" + providerName + " " + event.Detail
		}
		if err := devStore.AppendEvent(context.Background(), event); err != nil {
			log.Printf("evento dev: %v", err)
		}
	})
	gw := gateway.New(snap, rt)
	metricRegistry := keymetrics.New()
	defer gw.CloseLeases()
	gw.SetEventSink(func(event sched.Event) {
		if err := devStore.AppendEvent(context.Background(), event); err != nil {
			log.Printf("evento lease dev: %v", err)
		}
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	wm := warm.New(snap.WarmConfig(), rt, devStore)
	wm.SetUsageSink(func(event domain.UsageEvent) {
		if err := devStore.AppendUsage(context.Background(), []domain.UsageEvent{event}); err != nil {
			log.Printf("uso warm dev: %v", err)
		}
	})
	wm.SetEventSink(func(event sched.Event) {
		if err := devStore.AppendEvent(context.Background(), event); err != nil {
			log.Printf("evento warm dev: %v", err)
		}
	})
	gw.SetMetrics(metricRegistry, func(reg *keymetrics.Registry) { collectRuntimeMetrics(reg, rt, wm, nil) })
	gw.SetReadiness(false, "aguardando primeira passada L1")
	wm.SetOnReady(gw.SetReadiness)
	publish := func(next *config.Snapshot) error {
		if err := gw.SetSnapshot(next); err != nil {
			return err
		}
		rt.MarkStateDirty()
		wm.SetConfig(next.WarmConfig())
		return nil
	}
	adminAPI, adminErr := admin.New(devStore, raw.APIKey, publish)
	if adminErr != nil {
		return adminErr
	}
	adminAPI.SetWarmer(wm)
	gw.SetAdminHandler(adminAPI.Handler())
	gw.SetUsageSink(func(event domain.UsageEvent) {
		if err := devStore.AppendUsage(context.Background(), []domain.UsageEvent{event}); err != nil {
			log.Printf("uso dev: %v", err)
		}
	})
	if err := wm.Start(ctx); err != nil {
		return err
	}
	defer wm.Close()
	if generated {
		fmt.Println("KEYPOOL_API_KEY=" + raw.APIKey)
	}
	addr := snap.Listen()
	if !isLoopbackAddress(addr) {
		return fmt.Errorf("listen deve ser loopback; recebido: %s", addr)
	}
	srv := &http.Server{Addr: addr, Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { log.Printf("KeyPool dev ouvindo em http://%s", addr); errCh <- srv.ListenAndServe() }()
	select {
	case e := <-errCh:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			return e
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func retentionLoop(ctx context.Context, st *pgstore.Store) {
	prune := func() {
		jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := st.Prune(jobCtx, time.Now().UTC(), pgstore.Retention{}); err != nil && ctx.Err() == nil {
			log.Printf("retencao PostgreSQL: %v", err)
		}
		cancel()
	}
	prune()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			prune()
		case <-ctx.Done():
			return
		}
	}
}
func environment() map[string]string {
	out := map[string]string{}
	for _, v := range os.Environ() {
		p := strings.SplitN(v, "=", 2)
		if len(p) == 2 {
			out[p[0]] = p[1]
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
func masterKeyPath() string {
	if p := os.Getenv("KEYPOOL_MASTER_KEY_FILE"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "master.key")
}
func dataDir() string {
	if p := os.Getenv("KEYPOOL_DATA_DIR"); p != "" {
		return p
	}
	if p := os.Getenv("LOCALAPPDATA"); p != "" {
		return filepath.Join(p, "keypool")
	}
	if p := os.Getenv("XDG_CONFIG_HOME"); p != "" {
		return filepath.Join(p, "keypool")
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return ".keypool"
	}
	return filepath.Join(home, ".keypool")
}
func loadExistingMasterKey(path, env string) ([]byte, error) {
	if strings.TrimSpace(env) != "" {
		return cryptobox.MasterKeyFromEnv(env)
	}
	info, e := os.Stat(path)
	if e != nil {
		return nil, fmt.Errorf("master key indisponivel (%s): %w", path, e)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("permissoes inseguras em %s: %04o (esperado 0600)", path, info.Mode().Perm())
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, fmt.Errorf("master key indisponivel (%s): %w", path, e)
	}
	return cryptobox.MasterKeyFromEnv(string(b))
}
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
func usage() {
	fmt.Fprintln(os.Stderr, "Uso: keypoold <init|migrate|serve|doctor|version> [opcoes]")
	fmt.Fprintln(os.Stderr, "  migrate [--database URL]")
	fmt.Fprintln(os.Stderr, "  init [--database URL] [--master-key-file PATH] [--master-key-dpapi]")
	fmt.Fprintln(os.Stderr, "  serve [--config arquivo.yaml] [--database URL] [--master-key-dpapi] (ou --dev arquivo.yaml)")
	fmt.Fprintln(os.Stderr, "  doctor [--database URL] [--master-key-dpapi]")
}
