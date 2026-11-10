// Package secret mantém credenciais em um tipo cuja representação é sempre mascarada.
package secret

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Secret encapsula um segredo; sua representação nunca contém o valor original.
type Secret struct{ v string }

// New cria um segredo.
func New(s string) Secret { return Secret{v: s} }

// Reveal retorna o valor em claro. Use somente dentro do transporte do adaptador.
func (s Secret) Reveal() string { return s.v }

// IsZero informa se não há segredo.
func (s Secret) IsZero() bool { return s.v == "" }

// String devolve a máscara do segredo.
func (s Secret) String() string { return Mask(s.v) }

// GoString devolve a máscara para %#v.
func (s Secret) GoString() string { return Mask(s.v) }

// Format mascara a representação para qualquer verbo de fmt.
func (s Secret) Format(f fmt.State, verb rune) { _, _ = fmt.Fprint(f, Mask(s.v)) }

// MarshalJSON serializa somente a máscara.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprintf("%q", Mask(s.v))), nil }

// MarshalText serializa somente a máscara.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Mask(s.v)), nil }

// LogValue retorna um valor slog já mascarado.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Mask(s.v)) }

// Mask aplica o contrato do Mathlab: até oito caracteres viram asteriscos.
func Mask(s string) string {
	if s == "" {
		return ""
	}
	if len([]rune(s)) <= 8 {
		return strings.Repeat("*", len([]rune(s)))
	}
	r := []rune(s)
	return string(r[:4]) + "…" + string(r[len(r)-4:])
}

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`gsk_[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`sk-or-[A-Za-z0-9_\-]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`(?i)Bearer\s+\S+`),
	regexp.MustCompile(`(?i)([?&](?:key|api_key|token|access_token)=)[^&\s]+`),
	regexp.MustCompile(`kp_[A-Za-z0-9]+`),
}

// Redact remove credenciais conhecidas de mensagens externas.
func Redact(text string) string {
	for i, p := range patterns {
		replacement := "[redacted-key]"
		if i == 4 {
			replacement = "${1}[redacted-key]"
		}
		text = p.ReplaceAllString(text, replacement)
	}
	return text
}
