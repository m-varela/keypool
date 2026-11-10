package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// outputJSON is selected per invocation by the global --json flag.
var outputJSON bool

// takeJSONFlag accepts --json before or after the command without forcing every
// nested flag set to know about the global output option.
func takeJSONFlag(args []string) ([]string, bool) {
	filtered := make([]string, 0, len(args))
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" {
			jsonMode = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, jsonMode
}

func printAPIResponse(raw []byte) {
	formatted, err := formatAPIResponse(raw, outputJSON)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keypoolctl: resposta inválida:", err)
		return
	}
	_, _ = io.WriteString(os.Stdout, formatted)
}

func formatAPIResponse(raw []byte, asJSON bool) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) {
		return string(raw), nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if asJSON || containsOneTimeSecret(value) {
		pretty, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", err
		}
		return string(pretty) + "\n", nil
	}
	return renderTable(value), nil
}

// Some operations (notably apikey rotate) return a credential exactly once.
// Do not silently hide it in the human table: preserve the one-time response.
func containsOneTimeSecret(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if lower == "api_key" || lower == "apikey" {
				if text, ok := child.(string); ok && text != "" && text != "[redacted]" && text != "***" {
					return true
				}
			}
			if containsOneTimeSecret(child) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if containsOneTimeSecret(child) {
				return true
			}
		}
	}
	return false
}

func renderTable(value any) string {
	if envelope, ok := value.(map[string]any); ok {
		if data, exists := envelope["data"]; exists {
			return renderTable(data)
		}
	}
	switch data := value.(type) {
	case []any:
		if len(data) == 0 {
			return "Sem registros.\n"
		}
		rows := make([]map[string]any, 0, len(data))
		for _, item := range data {
			row, ok := item.(map[string]any)
			if !ok {
				return renderKeyValue("valor", data)
			}
			rows = append(rows, row)
		}
		return renderRows(rows)
	case map[string]any:
		return renderKeyValueMap(data)
	default:
		return fmt.Sprintln(data)
	}
}

// filterResponseRows filters an API envelope whose data field is an array of
// objects. It keeps every other top-level envelope field intact for --json.
func filterResponseRows(raw []byte, field, expected string) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	var rows []map[string]any
	if data, ok := envelope["data"]; !ok {
		return nil, fmt.Errorf("resposta da API não contém data")
	} else if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("campo data não é uma lista de objetos: %w", err)
	}
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if fmt.Sprint(row[field]) == expected {
			filtered = append(filtered, row)
		}
	}
	envelope["data"], _ = json.Marshal(filtered)
	return json.Marshal(envelope)
}

func renderRows(rows []map[string]any) string {
	set := map[string]bool{}
	for _, row := range rows {
		for key := range row {
			if !sensitiveColumn(key) {
				set[key] = true
			}
		}
	}
	columns := make([]string, 0, len(set))
	for key := range set {
		columns = append(columns, key)
	}
	sort.Strings(columns)
	if len(columns) == 0 {
		return "Registros recebidos; campos sensíveis omitidos.\n"
	}
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = min(32, max(4, len(column)))
	}
	values := make([][]string, len(rows))
	for rowIndex, row := range rows {
		values[rowIndex] = make([]string, len(columns))
		for colIndex, column := range columns {
			cell := displayCell(row[column])
			values[rowIndex][colIndex] = cell
			widths[colIndex] = min(32, max(widths[colIndex], min(32, len(cell))))
		}
	}
	var out strings.Builder
	writeTableLine(&out, columns, widths)
	separators := make([]string, len(columns))
	for i, width := range widths {
		separators[i] = strings.Repeat("-", width)
	}
	writeTableLine(&out, separators, widths)
	for _, row := range values {
		writeTableLine(&out, row, widths)
	}
	if rowsContainOmittedFields(rows, columns) {
		out.WriteString("Campos sensíveis omitidos; use --json somente em ambiente seguro para ver a resposta integral.\n")
	}
	return out.String()
}

func renderKeyValueMap(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	width := 0
	for _, key := range keys {
		width = max(width, min(32, len(key)))
	}
	var out strings.Builder
	for _, key := range keys {
		if sensitiveColumn(key) {
			continue
		}
		fmt.Fprintf(&out, "%-*s  %s\n", width, truncate(key, 32), displayCell(value[key]))
	}
	return out.String()
}

func renderKeyValue(key string, value any) string {
	return renderKeyValueMap(map[string]any{key: value})
}

func writeTableLine(out *strings.Builder, values []string, widths []int) {
	for i, value := range values {
		if i > 0 {
			out.WriteString("  ")
		}
		out.WriteString(fmt.Sprintf("%-*s", widths[i], truncate(value, widths[i])))
	}
	out.WriteByte('\n')
}

func displayCell(value any) string {
	if value == nil {
		return "-"
	}
	switch item := value.(type) {
	case string:
		return strings.ReplaceAll(strings.ReplaceAll(item, "\r", " "), "\n", " ")
	case map[string]any, []any:
		encoded, err := json.Marshal(item)
		if err != nil {
			return "<valor>"
		}
		return string(encoded)
	default:
		return fmt.Sprint(item)
	}
}

func sensitiveColumn(name string) bool {
	lower := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	switch lower {
	case "secret", "api_key", "apikey", "client_secret", "authorization", "master_key", "master_key_check":
		return true
	default:
		return false
	}
}

func rowsContainOmittedFields(rows []map[string]any, columns []string) bool {
	shown := make(map[string]bool, len(columns))
	for _, column := range columns {
		shown[column] = true
	}
	for _, row := range rows {
		for key := range row {
			if !shown[key] {
				return true
			}
		}
	}
	return false
}

func truncate(value string, width int) string {
	if width <= 0 || len(value) <= width {
		return value
	}
	if width <= 1 {
		return value[:width]
	}
	return value[:width-1] + "…"
}
