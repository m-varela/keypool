package provider

import (
	"bytes"
	"embed"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

//go:embed presets/*.yaml
var presetFS embed.FS

// PresetNames lista os presets embutidos.
func PresetNames() []string {
	entries, _ := presetFS.ReadDir("presets")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// LoadPreset lê e valida um preset embutido pelo nome do arquivo.
func LoadPreset(name string) (*Spec, error) {
	b, e := presetFS.ReadFile("presets/" + name)
	if e != nil {
		return nil, e
	}
	var s Spec
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if e = decoder.Decode(&s); e != nil {
		return nil, fmt.Errorf("provider: preset %s: %w", name, e)
	}
	if e = s.Validate(); e != nil {
		return nil, e
	}
	return &s, nil
}
