// Package admin implementa a API administrativa autenticada do KeyPool.
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"keypool/internal/config"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

const maxAdminBody = 8 << 20

// SnapshotPublisher publica uma configuração validada no gateway.
type SnapshotPublisher func(*config.Snapshot) error

// WarmController is the administrative surface of the F6 warm service.
type WarmController interface {
	Run(context.Context, string, string) (any, error)
	Status() any
	ProbeKey(context.Context, string) (any, error)
	EnableKey(string)
}

// API serializa mutações para que apply e CRUD nunca sobrescrevam mudanças concorrentes.
type API struct {
	mu      sync.Mutex
	keyMu   sync.RWMutex
	repo    store.Store
	apiKey  string
	publish SnapshotPublisher
	warmer  WarmController
}

// New cria a API administrativa. A autenticação é aplicada pelo gateway que a monta.
func New(repo store.Store, apiKey string, publish SnapshotPublisher) (*API, error) {
	if repo == nil || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("admin: store e API key são obrigatórios")
	}
	return &API{repo: repo, apiKey: apiKey, publish: publish}, nil
}

// Handler retorna as rotas /admin/v1. O gateway deve protegê-las com a mesma API key.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/apply", a.apply)
	mux.HandleFunc("GET /admin/v1/providers", a.providers)
	mux.HandleFunc("PUT /admin/v1/providers/{name}", a.providerPut)
	mux.HandleFunc("DELETE /admin/v1/providers/{name}", a.providerDelete)
	mux.HandleFunc("POST /admin/v1/providers/{name}/enable", a.providerEnable)
	mux.HandleFunc("POST /admin/v1/providers/{name}/disable", a.providerDisable)
	mux.HandleFunc("GET /admin/v1/projects", a.projects)
	mux.HandleFunc("PUT /admin/v1/projects/{name}", a.projectPut)
	mux.HandleFunc("DELETE /admin/v1/projects/{name}", a.projectDelete)
	mux.HandleFunc("PUT /admin/v1/projects/{name}/key-rules", a.projectKeyRules)
	mux.HandleFunc("PUT /admin/v1/projects/{name}/routes/{alias}", a.projectRoute)
	mux.HandleFunc("GET /admin/v1/keys", a.keys)
	mux.HandleFunc("POST /admin/v1/keys", a.keyAdd)
	mux.HandleFunc("PATCH /admin/v1/keys/{id}", a.keyPatch)
	mux.HandleFunc("DELETE /admin/v1/keys/{id}", a.keyDelete)
	mux.HandleFunc("POST /admin/v1/keys/{id}/enable", a.keyEnable)
	mux.HandleFunc("POST /admin/v1/keys/{id}/disable", a.keyDisable)
	mux.HandleFunc("POST /admin/v1/keys/{id}/test", a.keyTest)
	mux.HandleFunc("POST /admin/v1/apikey/rotate", a.apiKeyRotate)
	mux.HandleFunc("GET /admin/v1/apikey", a.apiKeyInfo)
	mux.HandleFunc("GET /admin/v1/stats", a.stats)
	mux.HandleFunc("GET /admin/v1/usage", a.usage)
	mux.HandleFunc("GET /admin/v1/events", a.events)
	mux.HandleFunc("POST /admin/v1/warm/run", a.warmRun)
	mux.HandleFunc("GET /admin/v1/warm/status", a.warmStatus)
	return redactResponses(a, mux)
}

// SetWarmer connects warm/run, warm/status and key/test to F6.
func (a *API) SetWarmer(w WarmController) { a.mu.Lock(); a.warmer = w; a.mu.Unlock() }

func (a *API) currentAPIKey() string { a.keyMu.RLock(); defer a.keyMu.RUnlock(); return a.apiKey }

func (a *API) load(ctx context.Context) (*config.Raw, error) {
	raw, err := a.repo.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var cloned config.Raw
	if err = json.Unmarshal(b, &cloned); err != nil {
		return nil, err
	}
	if cloned.Providers == nil {
		cloned.Providers = map[string]config.ProviderRaw{}
	}
	if cloned.Projects == nil {
		cloned.Projects = map[string]config.ProjectRaw{}
	}
	cloned.APIKey = a.currentAPIKey()
	keys, err := a.repo.LoadKeys(ctx)
	if err != nil {
		return nil, err
	}
	for name, p := range cloned.Providers {
		p.Keys = nil
		for _, key := range keys {
			if key.Provider == name && key.Enabled {
				p.Keys = append(p.Keys, sched.KeySpec{
					ID: key.ID, Label: key.Label, Secret: key.Secret,
					Group: key.Group, Tags: append([]string(nil), key.Tags...),
				})
			}
		}
		cloned.Providers[name] = p
	}
	return &cloned, nil
}

func (a *API) commit(ctx context.Context, raw *config.Raw) error {
	raw.APIKey = a.currentAPIKey()
	if err := validateNames(raw); err != nil {
		return err
	}
	if _, err := config.Build(raw); err != nil {
		return fmt.Errorf("configuração inválida: %w", err)
	}
	if err := a.repo.SaveConfig(ctx, raw); err != nil {
		return fmt.Errorf("persistir configuração: %w", err)
	}
	snapshot, err := config.Build(raw)
	if err != nil {
		return err
	}
	if a.publish != nil {
		if err = a.publish(snapshot); err != nil {
			return fmt.Errorf("publicar snapshot: %w", err)
		}
	}
	return nil
}

func validateNames(raw *config.Raw) error {
	for name := range raw.Providers {
		if !validName(name) {
			return fmt.Errorf("nome de provedor inválido: %q", name)
		}
	}
	for name, project := range raw.Projects {
		if !validName(name) {
			return fmt.Errorf("nome de projeto inválido: %q", name)
		}
		if project.Project != "" && project.Project != name {
			return fmt.Errorf("project %q diverge da chave %q", project.Project, name)
		}
	}
	return nil
}
func validName(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (a *API) apply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current, err := a.load(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if isProjectDocument(body) {
		var project config.ProjectRaw
		if err = decodeStrict(body, &project); err != nil {
			writeError(w, 400, fmt.Errorf("perfil de projeto inválido: %w", err))
			return
		}
		if strings.TrimSpace(project.Project) == "" {
			writeError(w, 400, errors.New("campo project obrigatório"))
			return
		}
		current.Projects[project.Project] = project
		if err = a.commit(r.Context(), current); err != nil {
			writeError(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "applied", "project": project.Project})
		return
	}
	var incoming config.Raw
	if err = decodeStrict(body, &incoming); err != nil {
		writeError(w, 400, fmt.Errorf("documento inválido: %w", err))
		return
	}
	// Apply é declarativo e não aceita credenciais. Chaves atuais são preservadas
	// somente para provedores que continuam presentes no documento.
	for name, p := range incoming.Providers {
		if old, ok := current.Providers[name]; ok {
			p.Keys = old.Keys
		}
		incoming.Providers[name] = p
	}
	if incoming.APIKey == "" {
		incoming.APIKey = a.currentAPIKey()
	}
	if err = a.commit(r.Context(), &incoming); err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "applied"})
}

func isProjectDocument(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(trimmed, &doc) != nil {
		return false
	}
	_, hasProject := doc["project"]
	_, hasProviders := doc["providers"]
	_, hasProjects := doc["projects"]
	return hasProject && !hasProviders && !hasProjects
}

func decodeStrict(body []byte, dst any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return errors.New("corpo vazio")
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return errors.New("formato inválido: use um documento JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("conteúdo adicional após documento JSON")
	}
	return nil
}

func (a *API) providers(w http.ResponseWriter, r *http.Request) {
	raw, err := a.load(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	names := make([]string, 0, len(raw.Providers))
	for n := range raw.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		p := raw.Providers[n]
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		out = append(out, map[string]any{
			"name": n, "type": p.Type, "preset": p.Preset,
			"enabled": enabled, "limits": p.Limits,
		})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}
func (a *API) providerPut(w http.ResponseWriter, r *http.Request) {
	var p config.ProviderRaw
	if err := decodeRequest(w, r, &p); err != nil {
		writeError(w, 400, err)
		return
	}
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		if strings.TrimSpace(name) == "" {
			return errors.New("nome do provedor vazio")
		}
		if old, ok := raw.Providers[name]; ok {
			p.Keys = old.Keys
		}
		raw.Providers[name] = p
		return nil
	})
}
func (a *API) providerDelete(w http.ResponseWriter, r *http.Request) {
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		if _, ok := raw.Providers[name]; !ok {
			return errors.New("provedor não encontrado")
		}
		delete(raw.Providers, name)
		return nil
	})
}
func (a *API) providerEnable(w http.ResponseWriter, r *http.Request) {
	a.setProviderEnabled(w, r, true)
}
func (a *API) providerDisable(w http.ResponseWriter, r *http.Request) {
	a.setProviderEnabled(w, r, false)
}
func (a *API) setProviderEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		p, ok := raw.Providers[name]
		if !ok {
			return errors.New("provedor não encontrado")
		}
		p.Enabled = &enabled
		raw.Providers[name] = p
		return nil
	})
}

func (a *API) projects(w http.ResponseWriter, r *http.Request) {
	raw, err := a.load(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	names := make([]string, 0, len(raw.Projects))
	for n := range raw.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]config.ProjectRaw, 0, len(names))
	for _, n := range names {
		out = append(out, raw.Projects[n])
	}
	writeJSON(w, 200, map[string]any{"data": out})
}
func (a *API) projectPut(w http.ResponseWriter, r *http.Request) {
	var p config.ProjectRaw
	if err := decodeRequest(w, r, &p); err != nil {
		writeError(w, 400, err)
		return
	}
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		p.Project = name
		raw.Projects[name] = p
		return nil
	})
}
func (a *API) projectDelete(w http.ResponseWriter, r *http.Request) {
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		if _, ok := raw.Projects[name]; !ok {
			return errors.New("projeto não encontrado")
		}
		delete(raw.Projects, name)
		return nil
	})
}
func (a *API) projectKeyRules(w http.ResponseWriter, r *http.Request) {
	var rules map[string]config.KeyRule
	if err := decodeRequest(w, r, &rules); err != nil {
		writeError(w, 400, err)
		return
	}
	a.mutate(w, r, func(raw *config.Raw) error {
		name := r.PathValue("name")
		p, ok := raw.Projects[name]
		if !ok {
			return errors.New("projeto não encontrado")
		}
		p.Keys = rules
		raw.Projects[name] = p
		return nil
	})
}
func (a *API) projectRoute(w http.ResponseWriter, r *http.Request) {
	var route config.RouteRaw
	if err := decodeRequest(w, r, &route); err != nil {
		writeError(w, 400, err)
		return
	}
	a.mutate(w, r, func(raw *config.Raw) error {
		name, alias := r.PathValue("name"), r.PathValue("alias")
		p, ok := raw.Projects[name]
		if !ok {
			return errors.New("projeto não encontrado")
		}
		if p.Routes == nil {
			p.Routes = map[string]config.RouteRaw{}
		}
		p.Routes[alias] = route
		raw.Projects[name] = p
		return nil
	})
}

func (a *API) keys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.repo.LoadKeys(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"id": k.ID, "provider": k.Provider, "label": k.Label,
			"mask": secret.Mask(k.Secret.Reveal()), "group": k.Group,
			"tags": k.Tags, "enabled": k.Enabled,
		})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

type keyInput struct {
	Provider string   `json:"provider"`
	Label    string   `json:"label"`
	Group    string   `json:"group"`
	Tags     []string `json:"tags"`
	Secret   string   `json:"secret"`
}

func (a *API) keyAdd(w http.ResponseWriter, r *http.Request) {
	var in keyInput
	if err := decodeRequest(w, r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	if in.Secret == "" || in.Provider == "" {
		writeError(w, 400, errors.New("provider e secret são obrigatórios"))
		return
	}
	id, err := newID()
	if err != nil {
		writeError(w, 500, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := a.load(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if _, ok := raw.Providers[in.Provider]; !ok {
		writeError(w, 404, errors.New("provedor não encontrado"))
		return
	}
	keys, err := a.repo.LoadKeys(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	for _, existing := range keys {
		if existing.Provider == in.Provider && existing.Secret.Reveal() == in.Secret {
			writeError(w, 409, errors.New("essa credencial já está cadastrada no provedor"))
			return
		}
		if existing.Provider == in.Provider && in.Label != "" && existing.Label == in.Label {
			writeError(w, 409, errors.New("já existe uma chave com esse rótulo"))
			return
		}
	}
	label := in.Label
	if label == "" {
		label = secret.Mask(in.Secret)
	}
	keys = append(keys, store.KeyRecord{
		Provider: in.Provider, ID: sched.KeyID(id), Label: label,
		Group: in.Group, Tags: in.Tags, Secret: secret.New(in.Secret), Enabled: true,
	})
	snapshot, buildErr := a.snapshotWithKeys(r.Context(), keys)
	if buildErr == nil {
		buildErr = a.repo.SaveKeys(r.Context(), in.Provider, filterProvider(keys, in.Provider))
	}
	if buildErr == nil && a.publish != nil {
		buildErr = a.publish(snapshot)
	}
	if buildErr != nil {
		writeError(w, 400, buildErr)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "provider": in.Provider, "label": label, "mask": secret.Mask(in.Secret)})
}

type keyPatch struct {
	Label   *string   `json:"label"`
	Group   *string   `json:"group"`
	Tags    *[]string `json:"tags"`
	Enabled *bool     `json:"enabled"`
}

func (a *API) keyPatch(w http.ResponseWriter, r *http.Request) {
	var patch keyPatch
	if err := decodeRequest(w, r, &patch); err != nil {
		writeError(w, 400, err)
		return
	}
	a.updateKey(w, r, func(k *store.KeyRecord) {
		if patch.Label != nil {
			k.Label = *patch.Label
		}
		if patch.Group != nil {
			k.Group = *patch.Group
		}
		if patch.Tags != nil {
			k.Tags = append([]string(nil), (*patch.Tags)...)
		}
		if patch.Enabled != nil {
			k.Enabled = *patch.Enabled
		}
	})
}
func (a *API) keyEnable(w http.ResponseWriter, r *http.Request) {
	if a.updateKey(w, r, func(k *store.KeyRecord) { k.Enabled = true }) && a.warmer != nil {
		a.warmer.EnableKey(r.PathValue("id"))
	}
}
func (a *API) keyDisable(w http.ResponseWriter, r *http.Request) {
	a.updateKey(w, r, func(k *store.KeyRecord) { k.Enabled = false })
}
func (a *API) keyDelete(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := sched.KeyID(r.PathValue("id"))
	keys, err := a.repo.LoadKeys(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	filtered := make([]store.KeyRecord, 0, len(keys))
	found := false
	for _, k := range keys {
		if k.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, k)
	}
	if !found {
		writeError(w, 404, errors.New("chave não encontrada"))
		return
	}
	snapshot, err := a.snapshotWithKeys(r.Context(), filtered)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if err = a.repo.DeleteKey(r.Context(), id); err != nil {
		writeError(w, 404, err)
		return
	}
	if a.publish != nil {
		if err = a.publish(snapshot); err != nil {
			writeError(w, 500, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"status": "deleted"})
}
func (a *API) updateKey(w http.ResponseWriter, r *http.Request, fn func(*store.KeyRecord)) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := sched.KeyID(r.PathValue("id"))
	keys, err := a.repo.LoadKeys(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return false
	}
	found := false
	providerName := ""
	for i := range keys {
		if keys[i].ID == id {
			fn(&keys[i])
			providerName = keys[i].Provider
			found = true
			break
		}
	}
	if !found {
		writeError(w, 404, errors.New("chave não encontrada"))
		return false
	}
	snapshot, buildErr := a.snapshotWithKeys(r.Context(), keys)
	if buildErr == nil {
		buildErr = a.repo.SaveKeys(r.Context(), providerName, filterProvider(keys, providerName))
	}
	if buildErr == nil && a.publish != nil {
		buildErr = a.publish(snapshot)
	}
	if buildErr != nil {
		writeError(w, 400, buildErr)
		return false
	}
	writeJSON(w, 200, map[string]any{"status": "ok"})
	return true
}
func filterProvider(keys []store.KeyRecord, provider string) []store.KeyRecord {
	out := make([]store.KeyRecord, 0)
	for _, k := range keys {
		if k.Provider == provider {
			out = append(out, k)
		}
	}
	return out
}
func (a *API) snapshotWithKeys(ctx context.Context, keys []store.KeyRecord) (*config.Snapshot, error) {
	raw, err := a.repo.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	raw.APIKey = a.currentAPIKey()
	for name, p := range raw.Providers {
		p.Keys = nil
		for _, k := range keys {
			if k.Provider == name && k.Enabled {
				p.Keys = append(p.Keys, sched.KeySpec{ID: k.ID, Label: k.Label, Secret: k.Secret, Group: k.Group, Tags: k.Tags})
			}
		}
		raw.Providers[name] = p
	}
	return config.Build(raw)
}

func (a *API) snapshotForAPIKey(ctx context.Context, apiKey string) (*config.Snapshot, error) {
	raw, err := a.repo.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	raw.APIKey = apiKey
	keys, err := a.repo.LoadKeys(ctx)
	if err != nil {
		return nil, err
	}
	for name, p := range raw.Providers {
		p.Keys = nil
		for _, k := range keys {
			if k.Provider == name && k.Enabled {
				p.Keys = append(p.Keys, sched.KeySpec{ID: k.ID, Label: k.Label, Secret: k.Secret, Group: k.Group, Tags: k.Tags})
			}
		}
		raw.Providers[name] = p
	}
	return config.Build(raw)
}
func (a *API) publishCurrent(ctx context.Context) error {
	snapshot, err := a.snapshotForAPIKey(ctx, a.currentAPIKey())
	if err != nil {
		return err
	}
	if a.publish != nil {
		return a.publish(snapshot)
	}
	return nil
}
func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	raw, err := a.load(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	keys, err := a.repo.LoadKeys(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	providers := make([]map[string]any, 0, len(raw.Providers))
	for name, p := range raw.Providers {
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		count, active := 0, 0
		for _, k := range keys {
			if k.Provider == name {
				count++
				if k.Enabled {
					active++
				}
			}
		}
		providers = append(providers, map[string]any{"name": name, "enabled": enabled, "keys": count, "active_keys": active})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i]["name"].(string) < providers[j]["name"].(string) })
	response := map[string]any{"providers": providers, "projects": len(raw.Projects), "keys": len(keys), "active_keys": countEnabled(keys)}
	if a.warmer != nil {
		response["warm"] = a.warmer.Status()
	}
	writeJSON(w, 200, response)
}
func countEnabled(keys []store.KeyRecord) int {
	n := 0
	for _, k := range keys {
		if k.Enabled {
			n++
		}
	}
	return n
}
func (a *API) usage(w http.ResponseWriter, r *http.Request) {
	reader, ok := a.repo.(store.AdminReader)
	if !ok {
		writeError(w, 501, errors.New("consulta de uso indisponível neste store"))
		return
	}
	from, err := parseTimeQuery(r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	to, err := parseTimeQuery(r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	rows, err := reader.ReadUsage(r.Context(), from, to, r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, 500, err)
		return
	}
	group := r.URL.Query().Get("group_by")
	if group != "" && group != "key" && group != "model" && group != "project" {
		writeError(w, 400, errors.New("group_by deve ser key, model ou project"))
		return
	}
	if group != "" {
		type agg struct {
			Requests         int `json:"requests"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		groups := map[string]*agg{}
		for _, e := range rows {
			key := e.Project
			if group == "key" {
				key = e.KeyID
			} else if group == "model" {
				key = e.Provider + ":" + e.Model
			}
			v := groups[key]
			if v == nil {
				v = &agg{}
				groups[key] = v
			}
			v.Requests++
			if e.Usage != nil {
				v.PromptTokens += e.Usage.PromptTokens
				v.CompletionTokens += e.Usage.CompletionTokens
			}
		}
		writeJSON(w, 200, map[string]any{"group_by": group, "data": groups})
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows})
}
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	reader, ok := a.repo.(store.AdminReader)
	if !ok {
		writeError(w, 501, errors.New("consulta de eventos indisponível neste store"))
		return
	}
	since := time.Time{}
	if value := r.URL.Query().Get("since"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			writeError(w, 400, errors.New("since deve ser duração positiva, ex.: 1h"))
			return
		}
		since = time.Now().Add(-d)
	}
	rows, err := reader.ReadEvents(r.Context(), r.URL.Query().Get("kind"), since)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows})
}
func (a *API) warmRun(w http.ResponseWriter, r *http.Request) {
	if a.warmer == nil {
		writeError(w, 501, errors.New("serviço de aquecimento indisponível"))
		return
	}
	var request struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if r.Body != nil {
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &request); err != nil {
				writeError(w, 400, err)
				return
			}
		}
	}
	result, err := a.warmer.Run(r.Context(), request.Provider, request.Key)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = 499
		}
		if errors.Is(err, context.DeadlineExceeded) {
			status = 504
		}
		if strings.Contains(err.Error(), "ocupada") {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "completed", "results": result})
}
func (a *API) warmStatus(w http.ResponseWriter, r *http.Request) {
	if a.warmer == nil {
		writeError(w, 501, errors.New("serviço de aquecimento indisponível"))
		return
	}
	writeJSON(w, 200, a.warmer.Status())
}
func parseTimeQuery(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("data deve estar em RFC3339: %w", err)
	}
	return t, nil
}

func (a *API) apiKeyInfo(w http.ResponseWriter, r *http.Request) {
	prefix := a.currentAPIKey()
	if len(prefix) > 7 {
		prefix = prefix[:7]
	}
	writeJSON(w, 200, map[string]any{"prefix": prefix, "configured": true})
}
func (a *API) apiKeyRotate(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, 500, err)
		return
	}
	token := "kp_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	oldToken := a.currentAPIKey()
	snapshot, err := a.snapshotForAPIKey(r.Context(), token)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if err = a.repo.RotateAPIKey(r.Context(), token); err != nil {
		writeError(w, 500, err)
		return
	}
	a.keyMu.Lock()
	a.apiKey = token
	a.keyMu.Unlock()
	if a.publish != nil {
		if err = a.publish(snapshot); err != nil {
			rollbackErr := a.repo.RotateAPIKey(r.Context(), oldToken)
			a.keyMu.Lock()
			a.apiKey = oldToken
			a.keyMu.Unlock()
			if rollbackErr != nil {
				writeError(w, 500, fmt.Errorf("publicar snapshot: %v; rollback da API key falhou: %w", err, rollbackErr))
			} else {
				writeError(w, 500, fmt.Errorf("publicar snapshot; rotação revertida: %w", err))
			}
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"api_key": token,
		"warning": "salve esta chave agora; ela não será exibida novamente",
	})
}
func (a *API) keyTest(w http.ResponseWriter, r *http.Request) {
	if a.warmer == nil {
		writeError(w, http.StatusNotImplemented, errors.New("serviço de sondagem L1 indisponível"))
		return
	}
	result, err := a.warmer.ProbeKey(r.Context(), r.PathValue("id"))
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "não encontrada") {
			status = http.StatusNotFound
		}
		if strings.Contains(err.Error(), "ocupada") {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "completed", "result": result})
}

func (a *API) mutate(w http.ResponseWriter, r *http.Request, fn func(*config.Raw) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := a.load(r.Context())
	if err == nil {
		err = fn(raw)
	}
	if err == nil {
		err = a.commit(r.Context(), raw)
	}
	if err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok"})
}
func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err != nil {
		return err
	}
	return decodeStrict(b, dst)
}
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
func redactResponses(api *API, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := &bufferedResponse{header: make(http.Header)}
		next.ServeHTTP(buf, r)
		body := buf.body.Bytes()
		if strings.Contains(strings.ToLower(buf.header.Get("Content-Type")), "application/json") {
			if keys, err := api.repo.LoadKeys(r.Context()); err == nil {
				for _, key := range keys {
					value := key.Secret.Reveal()
					if value != "" {
						body = bytes.ReplaceAll(body, []byte(value), []byte("[redacted-key]"))
					}
				}
			}
			// A rotação devolve intencionalmente a nova API key uma única vez.
			if r.URL.Path != "/admin/v1/apikey/rotate" {
				body = []byte(secret.Redact(string(body)))
			}
		}
		for name, values := range buf.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Del("Content-Length")
		status := buf.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"type": "admin_error", "message": secret.Redact(err.Error()),
	}})
}
