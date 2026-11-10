// keypoolctl é o cliente HTTP da API administrativa do KeyPool.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/store/memstore"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "keypoolctl:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	args, jsonMode := takeJSONFlag(args)
	previousOutputMode := outputJSON
	outputJSON = jsonMode
	defer func() { outputJSON = previousOutputMode }()
	if len(args) == 0 {
		return errors.New("uso: keypoolctl [--json] <status|provider|key|project|apikey|usage|events|warm|capacity>")
	}
	client := newClient()
	switch args[0] {
	case "status":
		paths := []string{"/admin/v1/stats", "/admin/v1/providers", "/admin/v1/keys", "/admin/v1/projects"}
		if outputJSON {
			result := make(map[string]json.RawMessage, len(paths))
			for _, path := range paths {
				body, err := client.request("GET", path, nil)
				if err != nil {
					return err
				}
				result[strings.TrimPrefix(path, "/admin/v1/")] = append(json.RawMessage(nil), body...)
			}
			body, err := json.Marshal(result)
			if err != nil {
				return err
			}
			printAPIResponse(body)
			return nil
		}
		for _, path := range paths {
			body, err := client.request("GET", path, nil)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, path)
			printAPIResponse(body)
		}
		return nil
	case "provider":
		return providerCommand(client, args[1:])
	case "key":
		return keyCommand(client, args[1:])
	case "project":
		return projectCommand(client, args[1:])
	case "apikey":
		return apiKeyCommand(client, args[1:])
	case "usage":
		return usageCommand(client, args[1:])
	case "events":
		return eventsCommand(client, args[1:])
	case "warm":
		return warmCommand(client, args[1:])
	case "capacity":
		fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
		project := fs.String("project", "", "projeto")
		alias := fs.String("alias", "", "alias")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		q := url.Values{}
		if *alias != "" {
			q.Set("alias", *alias)
		}
		path := "/v1/capacity"
		if q.Encode() != "" {
			path += "?" + q.Encode()
		}
		headers := map[string]string{}
		if *project != "" {
			headers["X-Keypool-Project"] = *project
		}
		b, err := client.requestWithHeaders("GET", path, nil, headers)
		printAPIResponse(b)
		return err
	default:
		return fmt.Errorf("comando desconhecido: %s", args[0])
	}
}

type apiClient struct {
	base, key string
	http      *http.Client
}

func newClient() *apiClient {
	base := os.Getenv("KEYPOOL_URL")
	if base == "" {
		base = "http://127.0.0.1:8787"
	}
	return &apiClient{
		base: strings.TrimRight(base, "/"), key: os.Getenv("KEYPOOL_API_KEY"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}
func (c *apiClient) request(method, path string, body []byte) ([]byte, error) {
	return c.requestWithHeaders(method, path, body, nil)
}
func (c *apiClient) requestWithHeaders(method, path string, body []byte, headers map[string]string) ([]byte, error) {
	if c.key == "" {
		return nil, errors.New("KEYPOOL_API_KEY ausente")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
func providerCommand(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("provider list|show <nome>|add -f arquivo|enable|disable <nome>")
	}
	switch args[0] {
	case "list":
		b, e := c.request("GET", "/admin/v1/providers", nil)
		printAPIResponse(b)
		return e
	case "show":
		if len(args) < 2 {
			return errors.New("provider show exige nome")
		}
		b, e := c.request("GET", "/admin/v1/providers", nil)
		if e != nil {
			return e
		}
		var payload struct {
			Data []map[string]any `json:"data"`
		}
		if e = json.Unmarshal(b, &payload); e != nil {
			return e
		}
		for _, item := range payload.Data {
			if item["name"] == args[1] {
				out, _ := json.MarshalIndent(item, "", "  ")
				printAPIResponse(out)
				return nil
			}
		}
		return errors.New("provedor não encontrado")
	case "add":
		fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
		file := fs.String("f", "", "arquivo JSON")
		name := fs.String("name", "", "nome do provedor (padrão: nome do arquivo)")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if *file == "" {
			return errors.New("use -f arquivo")
		}
		data, e := os.ReadFile(*file)
		if e != nil {
			return e
		}
		providerName := *name
		if providerName == "" {
			providerName = strings.TrimSuffix(filepath.Base(*file), filepath.Ext(*file))
		}
		b, e := c.request("PUT", "/admin/v1/providers/"+url.PathEscape(providerName), data)
		printAPIResponse(b)
		return e
	case "enable", "disable":
		if len(args) < 2 {
			return errors.New("nome do provedor ausente")
		}
		b, e := c.request("POST", "/admin/v1/providers/"+url.PathEscape(args[1])+"/"+args[0], nil)
		printAPIResponse(b)
		return e
	default:
		return fmt.Errorf("subcomando provider desconhecido: %s", args[0])
	}
}
func keyCommand(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("key list|add|import-env|enable|disable|tag|rm|test|reveal")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("key list", flag.ContinueOnError)
		providerName := fs.String("provider", "", "filtra por provedor")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if fs.NArg() != 0 {
			return errors.New("argumentos extras em key list")
		}
		b, e := c.request("GET", "/admin/v1/keys", nil)
		if e != nil {
			return e
		}
		if *providerName != "" {
			b, e = filterResponseRows(b, "provider", *providerName)
			if e != nil {
				return e
			}
		}
		printAPIResponse(b)
		return nil
	case "add":
		fs := flag.NewFlagSet("key add", flag.ContinueOnError)
		provider := fs.String("provider", "", "provedor")
		label := fs.String("label", "", "rótulo")
		group := fs.String("group", "", "grupo")
		stdin := fs.Bool("stdin", false, "ler segredo de stdin")
		var tags multiFlag
		fs.Var(&tags, "tag", "tag (repetível)")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if !*stdin {
			return errors.New("por segurança, informe --stdin para ler o segredo sem colocá-lo nos argumentos")
		}
		if *provider == "" {
			return errors.New("--provider obrigatório")
		}
		value, e := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if e != nil {
			return e
		}
		payload, _ := json.Marshal(map[string]any{
			"provider": *provider, "label": *label, "group": *group,
			"tags": []string(tags), "secret": strings.TrimSpace(string(value)),
		})
		b, e := c.request("POST", "/admin/v1/keys", payload)
		printAPIResponse(b)
		return e
	case "import-env":
		if len(args) < 2 {
			return errors.New("key import-env exige arquivo .env")
		}
		return importEnv(c, args[1:])
	case "enable", "disable", "rm", "test":
		if len(args) < 2 {
			return errors.New("identificador da chave ausente")
		}
		action := args[0]
		path := "/admin/v1/keys/" + url.PathEscape(args[1])
		method := "POST"
		if action == "rm" {
			method = "DELETE"
			action = ""
		}
		b, e := c.request(method, path+map[bool]string{true: "", false: "/" + action}[action == ""], nil)
		printAPIResponse(b)
		return e
	case "tag":
		if len(args) < 3 {
			return errors.New("key tag <id> +tag -tag")
		}
		current, err := c.request("GET", "/admin/v1/keys", nil)
		if err != nil {
			return err
		}
		var payload struct {
			Data []struct {
				ID   string   `json:"id"`
				Tags []string `json:"tags"`
			} `json:"data"`
		}
		if err = json.Unmarshal(current, &payload); err != nil {
			return err
		}
		var tags []string
		found := false
		for _, item := range payload.Data {
			if item.ID == args[1] {
				tags = append(tags, item.Tags...)
				found = true
				break
			}
		}
		if !found {
			return errors.New("chave não encontrada")
		}
		set := map[string]bool{}
		for _, tag := range tags {
			set[tag] = true
		}
		for _, op := range args[2:] {
			if len(op) < 2 {
				continue
			}
			if op[0] == '+' {
				set[op[1:]] = true
			} else if op[0] == '-' {
				delete(set, op[1:])
			}
		}
		tags = tags[:0]
		for tag := range set {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		body, _ := json.Marshal(map[string]any{"tags": tags})
		out, err := c.request("PATCH", "/admin/v1/keys/"+url.PathEscape(args[1]), body)
		printAPIResponse(out)
		return err
	case "reveal":
		if len(args) < 2 {
			return errors.New("key reveal exige label")
		}
		return revealLocal(args[1])
	default:
		return fmt.Errorf("subcomando key desconhecido: %s", args[0])
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
func importEnv(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("key import-env", flag.ContinueOnError)
	prefix := fs.String("prefix", "", "prefixo de ambiente")
	provider := fs.String("provider", "", "provedor destino")
	group := fs.String("group", "", "grupo")
	var tags multiFlag
	fs.Var(&tags, "tag", "tag")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *prefix == "" || *provider == "" {
		return errors.New("--prefix e --provider são obrigatórios")
	}
	path := args[0]
	if !filepath.IsAbs(path) {
		path = filepath.Clean(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	env := map[string]string{}
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, "\"'")
		env[strings.TrimSpace(parts[0])] = value
	}
	if err = scan.Err(); err != nil {
		return err
	}
	keys := config.KeysFromEnv(*prefix, env)
	if len(keys) == 0 {
		return errors.New("nenhuma chave encontrada com esse prefixo")
	}
	for _, k := range keys {
		payload, _ := json.Marshal(map[string]any{
			"provider": *provider, "label": k.Label, "group": *group,
			"tags":   append(append([]string{}, tags...), k.Tags...),
			"secret": k.Secret.Reveal(),
		})
		b, e := c.request("POST", "/admin/v1/keys", payload)
		if e != nil {
			return e
		}
		printAPIResponse(b)
	}
	return nil
}
func projectCommand(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("project apply -f arquivo | list | show <nome")
	}
	switch args[0] {
	case "apply":
		fs := flag.NewFlagSet("project apply", flag.ContinueOnError)
		file := fs.String("f", "", "arquivo declarativo")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		if *file == "" {
			return errors.New("use -f arquivo")
		}
		b, e := os.ReadFile(*file)
		if e != nil {
			return e
		}
		trimmed := bytes.TrimSpace(b)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return errors.New("o formato de apply agora é JSON; forneça um arquivo .json iniciado por '{'")
		}
		out, e := c.request("POST", "/admin/v1/apply", b)
		printAPIResponse(out)
		return e
	case "list":
		b, e := c.request("GET", "/admin/v1/projects", nil)
		if e == nil {
			printAPIResponse(b)
		}
		return e
	case "show":
		if len(args) < 2 {
			return errors.New("project show exige nome")
		}
		b, e := c.request("GET", "/admin/v1/projects", nil)
		if e != nil {
			return e
		}
		var payload struct {
			Data []map[string]any `json:"data"`
		}
		if e = json.Unmarshal(b, &payload); e != nil {
			return e
		}
		for _, item := range payload.Data {
			if item["project"] == args[1] {
				out, _ := json.MarshalIndent(item, "", "  ")
				printAPIResponse(out)
				return nil
			}
		}
		return errors.New("projeto não encontrado")
	default:
		return fmt.Errorf("subcomando project desconhecido: %s", args[0])
	}
}
func apiKeyCommand(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("apikey info|rotate")
	}
	switch args[0] {
	case "info":
		b, e := c.request("GET", "/admin/v1/apikey", nil)
		printAPIResponse(b)
		return e
	case "rotate":
		b, e := c.request("POST", "/admin/v1/apikey/rotate", nil)
		if e == nil {
			printAPIResponse(b)
		}
		return e
	default:
		return errors.New("subcomando apikey desconhecido")
	}
}
func revealLocal(label string) error {
	if strings.TrimSpace(label) == "" {
		return errors.New("reveal exige um rótulo")
	}
	var master []byte
	var err error
	if value := os.Getenv("KEYPOOL_MASTER_KEY"); value != "" {
		master, err = cryptobox.MasterKeyFromEnv(value)
	} else {
		path := masterKeyPath()
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("master key indisponível (%s): %w", path, readErr)
		}
		master, err = cryptobox.MasterKeyFromEnv(string(data))
	}
	if err != nil {
		return err
	}
	box, err := cryptobox.New(master)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Revelar o segredo de %q? Digite SIM para confirmar: ", label)
	var answer string
	if _, err = fmt.Fscan(os.Stdin, &answer); err != nil {
		return err
	}
	if answer != "SIM" {
		return errors.New("operação cancelada")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := memstore.Open(ctx, memstore.Options{Dir: dataDir(), Box: box, Clock: clock.Real()})
	if err != nil {
		return err
	}
	defer st.Close()
	if !st.Initialized() {
		return errors.New("state.json não existe; execute keypoold serve para inicializar o KeyPool")
	}
	keys, err := st.LoadKeys(ctx)
	if err != nil {
		return err
	}
	matches := []string{}
	for _, key := range keys {
		if key.Label == label {
			matches = append(matches, key.Secret.Reveal())
		}
	}
	if len(matches) == 0 {
		return errors.New("rótulo não encontrado")
	}
	if len(matches) > 1 {
		return errors.New("rótulo ambíguo; use um identificador único")
	}
	fmt.Println(matches[0])
	return nil
}

func dataDir() string {
	if path := os.Getenv("KEYPOOL_DATA_DIR"); path != "" {
		return path
	}
	if path := os.Getenv("LOCALAPPDATA"); path != "" {
		return filepath.Join(path, "keypool")
	}
	if path := os.Getenv("XDG_CONFIG_HOME"); path != "" {
		return filepath.Join(path, "keypool")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".keypool"
	}
	return filepath.Join(home, ".keypool")
}

func masterKeyPath() string {
	if path := os.Getenv("KEYPOOL_MASTER_KEY_FILE"); path != "" {
		return path
	}
	return filepath.Join(dataDir(), "master.key")
}

func usageCommand(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	project := fs.String("project", "", "projeto")
	since := fs.String("since", "24h", "janela de tempo")
	group := fs.String("group-by", "", "key|model|project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := time.ParseDuration(*since)
	if err != nil || d <= 0 {
		return errors.New("--since deve ser duração positiva")
	}
	q := url.Values{}
	q.Set("from", time.Now().Add(-d).Format(time.RFC3339))
	if *project != "" {
		q.Set("project", *project)
	}
	if *group != "" {
		q.Set("group_by", *group)
	}
	b, err := c.request("GET", "/admin/v1/usage?"+q.Encode(), nil)
	if err == nil {
		printAPIResponse(b)
	}
	return err
}
func eventsCommand(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	since := fs.String("since", "1h", "janela de tempo")
	kind := fs.String("kind", "", "tipo do evento")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("since", *since)
	if *kind != "" {
		q.Set("kind", *kind)
	}
	b, err := c.request("GET", "/admin/v1/events?"+q.Encode(), nil)
	if err == nil {
		printAPIResponse(b)
	}
	return err
}
func warmCommand(c *apiClient, args []string) error {
	if len(args) == 0 {
		return errors.New("warm run [--provider NAME] [--key ID]|status")
	}
	method, path := "GET", "/admin/v1/warm/status"
	var body []byte
	if args[0] == "run" {
		method, path = "POST", "/admin/v1/warm/run"
		fs := flag.NewFlagSet("warm run", flag.ContinueOnError)
		providerName := fs.String("provider", "", "filtra por provedor")
		keyID := fs.String("key", "", "filtra por ID da chave")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("argumentos extras em warm run")
		}
		var err error
		body, err = json.Marshal(map[string]string{"provider": *providerName, "key": *keyID})
		if err != nil {
			return err
		}
	} else if args[0] != "status" || len(args) != 1 {
		return errors.New("warm run [--provider NAME] [--key ID]|status")
	}
	b, err := c.request(method, path, body)
	if err == nil {
		printAPIResponse(b)
	}
	return err
}
