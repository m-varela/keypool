package memstore

import (
	"time"

	"keypool/internal/domain"
)

type dailyState struct {
	Day           string         `json:"day"`
	ProjectTokens map[string]int `json:"project_tokens,omitempty"`
	WarmL2Tokens  map[string]int `json:"warm_l2_tokens,omitempty"`
}

func newDaily(now time.Time) dailyState {
	return dailyState{Day: now.UTC().Format("2006-01-02"), ProjectTokens: map[string]int{}, WarmL2Tokens: map[string]int{}}
}

func cloneDaily(in dailyState) dailyState {
	out := dailyState{Day: in.Day, ProjectTokens: make(map[string]int, len(in.ProjectTokens)), WarmL2Tokens: make(map[string]int, len(in.WarmL2Tokens))}
	for key, value := range in.ProjectTokens {
		out.ProjectTokens[key] = value
	}
	for key, value := range in.WarmL2Tokens {
		out.WarmL2Tokens[key] = value
	}
	return out
}

func (d *dailyState) roll(now time.Time) bool {
	day := now.UTC().Format("2006-01-02")
	if d.Day != day {
		*d = newDaily(now)
		return true
	}
	if d.ProjectTokens == nil {
		d.ProjectTokens = map[string]int{}
	}
	if d.WarmL2Tokens == nil {
		d.WarmL2Tokens = map[string]int{}
	}
	return false
}

func totalEventTokens(event domain.UsageEvent) int {
	if event.ActualTokens > 0 {
		return event.ActualTokens
	}
	if event.Usage != nil {
		if event.Usage.TotalTokens > 0 {
			return event.Usage.TotalTokens
		}
		if total := event.Usage.PromptTokens + event.Usage.CompletionTokens; total > 0 {
			return total
		}
	}
	return event.EstimatedTokens
}

func isProjectUsage(source string) bool {
	if source == "" || (source != "warmup" && source != "lease" && source != "passthrough" && source != "request") {
		source = "request"
	}
	return source != "warmup"
}
