package cryptobox

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRoundTripAndAAD(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := b.Encrypt("id-1", "groq", []byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) < 1+12+16 || enc[0] != Version {
		t.Fatalf("envelope invalido: %x", enc)
	}
	got, err := b.Decrypt("id-1", "groq", enc)
	if err != nil || string(got) != "secret-value" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}
	if _, err = b.Decrypt("id-2", "groq", enc); err == nil {
		t.Fatal("AAD id diferente aceito")
	}
	if _, err = b.Decrypt("id-1", "openrouter", enc); err == nil {
		t.Fatal("AAD provider diferente aceito")
	}
	enc[len(enc)-1] ^= 1
	if _, err = b.Decrypt("id-1", "groq", enc); err == nil {
		t.Fatal("ciphertext adulterado aceito")
	}
}
func TestMasterKeyCheck(t *testing.T) {
	a, _ := New(bytes.Repeat([]byte{1}, 32))
	b, _ := New(bytes.Repeat([]byte{2}, 32))
	check, err := a.MasterKeyCheck()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyMasterKey(check); err != nil {
		t.Fatal(err)
	}
	if err := b.VerifyMasterKey(check); err == nil {
		t.Fatal("master key incorreta aceita")
	}
}
func TestMasterKeyFromEnv(t *testing.T) {
	k := bytes.Repeat([]byte{3}, 32)
	enc := "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	got, err := MasterKeyFromEnv(enc)
	if err != nil || !bytes.Equal(got, k) {
		t.Fatalf("%x %v", got, err)
	}
}

func TestLoadOrCreateMasterKeyDoesNotOverwrite(t *testing.T) {
	path := t.TempDir() + "/master.key"
	first, err := LoadOrCreateMasterKey(path, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateMasterKey(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("master key mudou entre leituras")
	}
	override := bytes.Repeat([]byte{9}, 32)
	if _, err := LoadOrCreateMasterKey(path, "CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk="); err != nil {
		t.Fatal(err)
	}
	persisted, err := LoadOrCreateMasterKey(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, persisted) || bytes.Equal(override, persisted) {
		t.Fatal("arquivo existente foi sobrescrito")
	}
}

func TestLoadOrCreateMasterKeyRejectsInsecureUnixPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows usa ACL, nao bits POSIX")
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := LoadOrCreateMasterKey(path, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMasterKey(path, ""); err == nil {
		t.Fatal("arquivo com permissao 0644 foi aceito")
	}
}
