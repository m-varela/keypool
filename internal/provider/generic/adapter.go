// Package generic fornece um transporte HTTP declarativo mínimo para provedores adicionais.
package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"keypool/internal/classify"
	"keypool/internal/provider"
	"keypool/internal/provider/openaicompat"
	"keypool/internal/sched"
	"keypool/internal/secret"
)

// Adapter fornece classificação e hints para generic_http. O tráfego desse tipo
// usa exclusivamente o endpoint passthrough /p/{provider}/*, nunca a API de chat.
type Adapter struct{ compat openaicompat.Adapter }

func (Adapter) Type() string { return "generic_http" }
func init()                  { provider.Register("generic_http", func() provider.Adapter { return Adapter{} }) }
func (a Adapter) BuildRequest(ctx context.Context, s *provider.Spec, k secret.Secret, r *provider.Canonical) (*http.Request, error) {
	return nil, fmt.Errorf("provider: generic_http não aceita chat completions; use /p/{provider}/*")
}
func (a Adapter) Classify(resp *http.Response, b []byte, err error) classify.Result {
	return a.compat.Classify(resp, b, err)
}
func (a Adapter) Hints(h http.Header, now time.Time) *sched.Hints { return a.compat.Hints(h, now) }
func (a Adapter) Probe(ctx context.Context, c *http.Client, s *provider.Spec, k secret.Secret) provider.ProbeResult {
	return a.compat.Probe(ctx, c, s, k)
}
func (a Adapter) ListModels(ctx context.Context, c *http.Client, s *provider.Spec, k secret.Secret) ([]string, error) {
	return a.compat.ListModels(ctx, c, s, k)
}
func (a Adapter) AdaptPayload(s *provider.Spec, m string, p map[string]json.RawMessage) map[string]json.RawMessage {
	return a.compat.AdaptPayload(s, m, p)
}

func (a Adapter) HintsFor(s *provider.Spec, h http.Header, now time.Time) *sched.Hints {
	return a.compat.HintsFor(s, h, now)
}
