// Package domain contém tipos puros compartilhados entre os componentes do KeyPool.
package domain

import "time"

// Outcome classifica o resultado de uma chamada ao provedor.
type Outcome string

const (
	Success          Outcome = "success"
	ClientError      Outcome = "client_error"
	AuthInvalid      Outcome = "auth_invalid"
	Forbidden        Outcome = "forbidden"
	QuotaExhausted   Outcome = "quota_exhausted"
	ModelUnavailable Outcome = "model_unavailable"
	Transient        Outcome = "transient"
	RateLimitedShort Outcome = "rate_limited_short"
	RateLimitedLong  Outcome = "rate_limited_long"
	Canceled         Outcome = "canceled"
)

// Target identifica um modelo em um provedor.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (t Target) String() string { return t.Provider + ":" + t.Model }

// Usage descreve a contagem de tokens informada pelo provedor.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UsageEvent é o registro mínimo de uma tentativa; nunca contém prompt ou resposta.
type UsageEvent struct {
	At              time.Time `json:"at"`
	RequestID       string    `json:"request_id"`
	Project         string    `json:"project"`
	Alias           string    `json:"alias"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	KeyID           string    `json:"key_id,omitempty"`
	Outcome         string    `json:"outcome"`
	Source          string    `json:"source,omitempty"`
	Status          int       `json:"status,omitempty"`
	Attempt         int       `json:"attempt"`
	QueueMS         int       `json:"queue_ms"`
	EstimatedTokens int       `json:"estimated_tokens,omitempty"`
	ActualTokens    int       `json:"actual_tokens,omitempty"`
	Usage           *Usage    `json:"usage,omitempty"`
}
