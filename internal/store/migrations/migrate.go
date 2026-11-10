// Package migrations carrega SQLs numerados embutidos e calcula checksums.
// A execucao e especifica do PostgreSQL e fica isolada em internal/store/pgstore.
package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Migration e um arquivo SQL imutavel identificado pelo nome e checksum.
type Migration struct {
	Name     string
	SQL      string
	Checksum string
}

// List retorna as migracoes SQL embutidas em ordem lexicografica.
func List() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: listar SQLs: %w", err)
	}
	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: ler %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{Name: entry.Name(), SQL: string(b), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
