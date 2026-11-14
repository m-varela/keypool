package classify

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyStatusMatrix(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Outcome
	}{{200, `{"choices":[]}`, Success}, {400, `{"error":{"message":"bad request"}}`, ClientError}, {401, `{"error":{"message":"invalid"}}`, AuthInvalid}, {402, `{"error":{"message":"quota"}}`, QuotaExhausted}, {403, `{"error":{"message":"credit limit reached"}}`, QuotaExhausted}, {403, `{"error":{"message":"not allowed"}}`, Forbidden}, {404, `{"error":{"message":"missing"}}`, ModelUnavailable}, {408, ``, Transient}, {503, ``, Transient}, {429, ``, RateLimitedShort}, {200, `{"error":{"message":"bad"}}`, Transient}}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+string(tc.want), func(t *testing.T) {
			got := Classify(tc.status, []byte(tc.body), nil, nil, time.Now())
			if got.Outcome != tc.want {
				t.Fatalf("got %s want %s", got.Outcome, tc.want)
			}
		})
	}
}

func TestDailyQuota429IsExhaustedNotShortThrottle(t *testing.T) {
	cases := []string{
		`{"error":{"message":"Rate limit reached: tokens per day (TPD) exceeded"}}`,
		`{"error":{"message":"Quota exceeded for GenerateRequestsPerDayPerProjectPerModel-FreeTier"}}`,
		`{"error":{"message":"daily request quota exhausted"}}`,
		`{"error":{"message":"daily limit exceeded"}}`,
		`{"error":{"message":"free-models-per-day limit reached"}}`,
		`{"error":{"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20"}}`,
	}
	for _, body := range cases {
		got := Classify(http.StatusTooManyRequests, []byte(body), nil, nil, time.Now())
		if got.Outcome != QuotaExhausted {
			t.Fatalf("daily quota body %q classified as %s", body, got.Outcome)
		}
	}
	forbiddenQuota := Classify(http.StatusForbidden, []byte(`{"error":{"message":"daily quota limit exceeded"}}`), nil, nil, time.Now())
	if forbiddenQuota.Outcome != QuotaExhausted {
		t.Fatalf("daily quota 403 classified as %s", forbiddenQuota.Outcome)
	}
	short := Classify(http.StatusTooManyRequests, []byte(`{"error":{"message":"requests per minute exceeded"}}`), nil, nil, time.Now())
	if short.Outcome != RateLimitedShort {
		t.Fatalf("short throttle classified as %s", short.Outcome)
	}
	geminiMinute := Classify(http.StatusTooManyRequests, []byte(`{"error":{"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests_per_minute"}}`), nil, nil, time.Now())
	if geminiMinute.Outcome != RateLimitedShort {
		t.Fatalf("Gemini per-minute metric classified as %s; want short throttle", geminiMinute.Outcome)
	}
}
func TestRetryAfterFormats(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name, value string
		want        time.Duration
	}{{"x-ratelimit-reset-tokens", "7.66s", 7660 * time.Millisecond}, {"x-ratelimit-reset-requests", "2m59.56s", 179560 * time.Millisecond}, {"x-ratelimit-reset-tokens", "14h12m", 14*time.Hour + 12*time.Minute}, {"x-ratelimit-reset-tokens", "120ms", 120 * time.Millisecond}}
	for _, tc := range cases {
		h := http.Header{}
		h.Set(tc.name, tc.value)
		got, ok := RetryAfter(h, now)
		if !ok || got != tc.want {
			t.Fatalf("%s=%s got %s %v", tc.name, tc.value, got, ok)
		}
	}
	h := http.Header{"Retry-After": []string{"999999"}}
	got, ok := RetryAfter(h, now)
	if !ok || got != 24*time.Hour {
		t.Fatalf("clamp: %s %v", got, ok)
	}
}
func TestRetryAfterEpochSecondsAndMillis(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, v := range []string{"1700000060", "1700000060000"} {
		h := http.Header{"X-Ratelimit-Reset": []string{v}}
		d, ok := RetryAfter(h, now)
		if !ok || d < 59*time.Second || d > 61*time.Second {
			t.Fatalf("%s => %s %v", v, d, ok)
		}
	}
}
func FuzzRetryAfter(f *testing.F) {
	f.Add("12.5")
	f.Add("2m59.56s")
	f.Fuzz(func(t *testing.T, s string) {
		h := http.Header{"Retry-After": []string{s}}
		_, _ = RetryAfter(h, time.Unix(0, 0))
	})
}

func TestSuccessfulStatusWithInvalidJSONIsTransient(t *testing.T) {
	for _, body := range []string{"not-json", "", `{"choices":[`} {
		got := Classify(http.StatusOK, []byte(body), nil, nil, time.Now())
		if got.Outcome != Transient {
			t.Fatalf("body %q classified as %s", body, got.Outcome)
		}
	}
	if got := Classify(http.StatusNoContent, nil, nil, nil, time.Now()); got.Outcome != Success {
		t.Fatalf("204 should be successful: %s", got.Outcome)
	}
}
