// Package classify transforma respostas HTTP de provedores em resultados comuns.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"keypool/internal/domain"
	"keypool/internal/secret"
)

type Outcome = domain.Outcome

const (
	Success          = domain.Success
	ClientError      = domain.ClientError
	AuthInvalid      = domain.AuthInvalid
	Forbidden        = domain.Forbidden
	QuotaExhausted   = domain.QuotaExhausted
	ModelUnavailable = domain.ModelUnavailable
	Transient        = domain.Transient
	RateLimitedShort = domain.RateLimitedShort
	RateLimitedLong  = domain.RateLimitedLong
	Canceled         = domain.Canceled
)

type Usage = domain.Usage

// Result transporta classificação, retry e uso sem preservar o corpo externo.
type Result struct {
	Outcome    Outcome       `json:"outcome"`
	Status     int           `json:"status"`
	RetryAfter time.Duration `json:"retry_after"`
	RetryKnown bool          `json:"retry_known"`
	Message    string        `json:"message"`
	Usage      *Usage        `json:"usage,omitempty"`
}

// Classify aplica a política comum do plano §9.6.
func Classify(status int, body []byte, h http.Header, err error, now time.Time) Result {
	if err != nil {
		if errors.Is(err, contextCanceled) {
			return Result{Outcome: Canceled, Message: "requisição cancelada"}
		}
		return Result{Outcome: Transient, Message: secret.Redact(err.Error())}
	}
	if status >= 200 && status < 300 && status != http.StatusNoContent {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || !json.Valid(trimmed) {
			return Result{Outcome: Transient, Status: status, Message: "resposta JSON inválida do provedor"}
		}
	}
	msg, code := errorMessage(body)
	if code > 0 {
		status = code
	}
	if status >= 200 && status < 300 && (hasErrorObject(body) || hasErrorFinishReason(body)) {
		if msg == "" {
			msg = "o provedor retornou erro em uma resposta de sucesso"
		}
		return Result{Outcome: Transient, Status: status, Message: secret.Redact(msg), Usage: parseUsage(body)}
	}
	out := Outcome("")
	switch status {
	case 0:
		out = Transient
	case 200, 201, 202, 204:
		out = Success
	case 400, 413, 422:
		out = ClientError
	case 401:
		out = AuthInvalid
	case 402:
		out = QuotaExhausted
	case 403:
		if containsDailyQuota(msg, body) || containsBudget(msg) {
			out = QuotaExhausted
		} else {
			out = Forbidden
		}
	case 404:
		out = ModelUnavailable
	case 408:
		out = Transient
	case 429:
		if containsDailyQuota(msg, body) {
			return Result{Outcome: QuotaExhausted, Status: status, Message: secret.Redact(msg), Usage: parseUsage(body)}
		}
		retry, known := RetryAfter(h, now)
		if !known || retry < 120*time.Second {
			out = RateLimitedShort
		} else {
			out = RateLimitedLong
		}
		return Result{Outcome: out, Status: status, RetryAfter: retry, RetryKnown: known, Message: secret.Redact(msg), Usage: parseUsage(body)}
	default:
		if status >= 500 {
			out = Transient
		} else if status >= 400 {
			out = ClientError
		} else if status >= 200 && status < 300 {
			out = Success
		} else {
			out = Transient
		}
	}
	return Result{Outcome: out, Status: status, Message: secret.Redact(msg), Usage: parseUsage(body)}
}

var contextCanceled = context.Canceled

func errorMessage(body []byte) (string, int) {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Code    int             `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &v) != nil {
		return strings.TrimSpace(string(body)), 0
	}
	msg := v.Message
	code := v.Code
	if len(v.Error) > 0 {
		var e struct {
			Message  string          `json:"message"`
			Code     json.RawMessage `json:"code"`
			Type     string          `json:"type"`
			Metadata map[string]any  `json:"metadata"`
		}
		if json.Unmarshal(v.Error, &e) == nil {
			if e.Message != "" {
				msg = e.Message
			}
			if n, ok := toInt(e.Code); ok && n > 0 {
				code = n
			}
		}
	}
	return secret.Redact(msg), code
}
func toInt(v json.RawMessage) (int, bool) {
	var n int
	if json.Unmarshal(v, &n) == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		n, e := strconv.Atoi(s)
		return n, e == nil
	}
	return 0, false
}
func containsBudget(s string) bool {
	s = strings.ToLower(s)
	for _, v := range []string{"budget", "limit", "credit", "spend"} {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// containsDailyQuota recognizes provider messages that identify a daily quota
// bucket. Such a response should move to another provider immediately instead
// of retrying the same upstream as if it were a short RPM/TPM throttle.
func containsDailyQuota(message string, body []byte) bool {
	text := strings.ToLower(message + " " + string(body))
	normalized := strings.NewReplacer(" ", "", "_", "", "-", "", ".", "").Replace(text)
	for _, marker := range []string{"perday", "dailyquota", "dailylimit", "dailyrequest", "dailytoken", "requestsperday", "tokensperday", "requestperday", "tokenperday", "generaterequestsperday", "generatetokensperday"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	// Gemini can identify its daily free-tier request cap by metric name without
	// spelling out "per day". Do not confuse the separate per-minute metric.
	if strings.Contains(normalized, "freetierrequests") && !strings.Contains(normalized, "perminute") {
		return true
	}
	return false
}
func parseUsage(b []byte) *Usage {
	var v struct {
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v.Usage
}

// RetryAfter lê Retry-After, reset de dimensão ou epoch absoluto.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	for _, name := range []string{"Retry-After", "retry-after"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if n, e := strconv.ParseFloat(v, 64); e == nil {
				d := time.Duration(n * float64(time.Second))
				return clampRetry(d), true
			}
		}
	}
	for _, name := range []string{"x-ratelimit-reset-tokens", "x-ratelimit-reset-requests", "X-RateLimit-Reset"} {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if d, e := time.ParseDuration(v); e == nil {
			return clampRetry(d), true
		}
		if f, e := strconv.ParseFloat(v, 64); e == nil {
			if strings.EqualFold(name, "X-RateLimit-Reset") {
				sec := f
				if f > 1e12 {
					sec = f / 1000
				}
				d := time.Unix(int64(sec), int64((sec-float64(int64(sec)))*1e9)).Sub(now)
				if d < 0 {
					d = 0
				}
				return clampRetry(d), true
			}
			return clampRetry(time.Duration(f * float64(time.Second))), true
		}
	}
	return 0, false
}
func clampRetry(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func hasErrorObject(body []byte) bool {
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	raw, ok := v["error"]
	if !ok || string(raw) == "null" || len(raw) == 0 {
		return false
	}
	return true
}

func hasErrorFinishReason(body []byte) bool {
	var v struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	for _, c := range v.Choices {
		if strings.EqualFold(c.FinishReason, "error") {
			return true
		}
	}
	return false
}
