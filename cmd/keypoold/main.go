// keypoold inicia e administra o gateway local KeyPool.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"time"

	"keypool/internal/clock"
	"keypool/internal/cryptobox"
	"keypool/internal/store/memstore"
)

const version = "0.4.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
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

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "arquivo JSON usado somente no primeiro início")
	useDPAPI := flags.Bool("master-key-dpapi", false, "proteger a master key com DPAPI no Windows")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("argumentos posicionais não são aceitos por serve")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	running, err := newApp(ctx, appOptions{ConfigPath: *configPath, MasterKeyDPAPI: *useDPAPI, Environ: environment(), Clock: clock.Real()})
	if err != nil {
		return err
	}
	if token, generated := running.GeneratedAPIKey(); generated {
		fmt.Println("KEYPOOL_API_KEY=" + token)
	}
	return running.Run(ctx)
}

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	useDPAPI := flags.Bool("master-key-dpapi", false, "ler master key protegida pelo DPAPI no Windows")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("argumentos posicionais não são aceitos por doctor")
	}
	env := environment()
	path := masterKeyPathFor(env, dataDirFor(env))
	master, err := readOnlyMasterKey(path, env["KEYPOOL_MASTER_KEY"], *useDPAPI)
	if err != nil {
		return err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := memstore.Open(ctx, memstore.Options{Dir: dataDirFor(env), Box: box, Clock: clock.Real()})
	if err != nil {
		return err
	}
	defer st.Close()
	if !st.Initialized() {
		return errors.New("state.json não existe; execute keypoold serve --config <arquivo.json>")
	}
	fmt.Println("[OK] state.json legível e master key válida")
	prefix, created, rotated, err := st.APIKeyInfo(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("[OK] API key configurada (%s; criada %s)", prefix, created.Format(time.RFC3339))
	if !rotated.IsZero() {
		fmt.Printf("; rotacionada %s", rotated.Format(time.RFC3339))
	}
	fmt.Println()
	if token := env["KEYPOOL_API_KEY"]; token != "" {
		valid, verifyErr := st.VerifyAPIKey(ctx, token)
		if verifyErr != nil {
			return verifyErr
		}
		if !valid {
			return errors.New("KEYPOOL_API_KEY do ambiente não corresponde ao hash persistido")
		}
		fmt.Println("[OK] KEYPOOL_API_KEY do ambiente")
	} else {
		fmt.Println("[AVISO] KEYPOOL_API_KEY não definida no ambiente")
	}
	if *useDPAPI {
		fmt.Println("[OK] Master key protegida pelo DPAPI do usuário atual")
	} else if env["KEYPOOL_MASTER_KEY"] != "" {
		fmt.Println("[OK] Master key fornecida por KEYPOOL_MASTER_KEY")
	} else if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("permissões inseguras em %s: %04o (esperado 0600)", path, info.Mode().Perm())
		}
		fmt.Println("[OK] Permissões Unix da master key")
	} else {
		fmt.Println("[INFO] ACL Windows: execute scripts/windows/acl-master-key.ps1")
	}
	raw, err := st.LoadConfig(ctx)
	if err != nil {
		return err
	}
	addr := raw.Listen
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if !isLoopbackAddress(addr) {
		return fmt.Errorf("listen não é loopback: %s", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("porta %s indisponível: %w", addr, err)
	}
	_ = listener.Close()
	fmt.Printf("[OK] Endereço %s disponível\n", addr)
	fmt.Printf("[INFO] Runtime Go: %s\n", runtime.Version())
	fmt.Println("Doctor finalizado.")
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "Uso: keypoold <serve|doctor|version> [opções]")
	fmt.Fprintln(os.Stderr, "  serve [--config arquivo.json] [--master-key-dpapi]")
	fmt.Fprintln(os.Stderr, "  doctor [--master-key-dpapi]")
}
