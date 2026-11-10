package secret

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretNeverFormatsPlaintext(t *testing.T) {
	raw := "gsk_abcdefghijklmnopqrstuvwxyz"
	s := New(raw)
	for _, got := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s), fmt.Sprintf("%q", s)} {
		if strings.Contains(got, raw) {
			t.Fatalf("leaked: %s", got)
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), raw) {
		t.Fatal("json leaked")
	}
	tb, err := s.MarshalText()
	if err != nil || strings.Contains(string(tb), raw) {
		t.Fatal("text leaked")
	}
	_ = slog.StringValue(s.String())
}
func TestMask(t *testing.T) {
	for in, want := range map[string]string{"": "", "12345678": "********", "123456789": "1234…6789", "gsk_abcdefghijklmnop": "gsk_…mnop"} {
		if got := Mask(in); got != want {
			t.Fatalf("Mask(%q)=%q, want %q", in, got, want)
		}
	}
}
func TestRedact(t *testing.T) {
	s := "oops gsk_abcdefgh sk-or-1234 sk-12345678901234567890 Bearer secret kp_Ab12 https://x.test/?api_key=hidden"
	got := Redact(s)
	for _, part := range []string{"gsk_abcdefgh", "sk-or-1234", "sk-12345678901234567890", "Bearer secret", "kp_Ab12", "api_key=hidden"} {
		if strings.Contains(got, part) {
			t.Fatalf("not redacted: %q in %q", part, got)
		}
	}
}
func FuzzRedact(f *testing.F) {
	f.Add("Bearer abc")
	f.Add("gsk_123")
	f.Fuzz(func(t *testing.T, s string) { _ = Redact(s) })
}
