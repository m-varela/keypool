package provider

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

//go:embed presets/*.json
var presetFS embed.FS

// PresetNames lista os presets embutidos.
func PresetNames() []string {
	entries, _ := presetFS.ReadDir("presets")
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out
}

// LoadPreset lê e valida um preset JSON embutido pelo nome do arquivo.
func LoadPreset(name string) (*Spec, error) {
	data, err := presetFS.ReadFile("presets/" + name)
	if err != nil {
		return nil, err
	}
	var spec Spec
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("provider: preset %s: %w", name, err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("provider: preset %s contém conteúdo adicional", name)
	}
	if err = spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}
