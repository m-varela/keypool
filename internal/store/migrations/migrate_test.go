package migrations

import "testing"

func TestEmbeddedMigrationsAreOrderedAndChecksummed(t *testing.T) {
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "0001_init.sql" || got[1].Name != "0002_config_runtime.sql" || got[2].Name != "0003_f7_usage_actual_tokens.sql" {
		t.Fatalf("migracoes inesperadas: %#v", got)
	}
	if got[0].Checksum != "7e8b7c666f6f954dd7a782ae198e462c19257273c0acdaa06075c0041f6a515c" {
		t.Fatalf("0001_init.sql e imutavel; checksum divergente: %s", got[0].Checksum)
	}
	for i, m := range got {
		if m.Name == "" || len(m.Checksum) != 64 || m.SQL == "" {
			t.Fatalf("migracao incompleta: %#v", m)
		}
		if i > 0 && got[i-1].Name >= m.Name {
			t.Fatalf("ordem invalida: %s antes de %s", got[i-1].Name, m.Name)
		}
	}
}
