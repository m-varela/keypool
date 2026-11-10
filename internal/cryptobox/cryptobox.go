// Package cryptobox cifra segredos persistidos usando AES-256-GCM.
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const Version byte = 1
const nonceSize = 12

var ErrInvalidKey = errors.New("cryptobox: master key invalida")
var ErrInvalidEnvelope = errors.New("cryptobox: envelope invalido ou autenticacao falhou")

// Box encapsula uma master key de 32 bytes.
type Box struct{ aead cipher.AEAD }

// New cria uma caixa AES-256-GCM.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Encrypt usa AAD id||provider para impedir troca de credenciais entre registros.
func (b *Box) Encrypt(id, provider string, plaintext []byte) ([]byte, error) {
	if b == nil || b.aead == nil || id == "" || provider == "" {
		return nil, errors.New("cryptobox: contexto ausente")
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 1, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out[0] = Version
	out = append(out, nonce...)
	out = b.aead.Seal(out, nonce, plaintext, aad(id, provider))
	return out, nil
}

// Decrypt valida versão, nonce e tag antes de devolver o segredo.
func (b *Box) Decrypt(id, provider string, envelope []byte) ([]byte, error) {
	if b == nil || b.aead == nil || id == "" || provider == "" || len(envelope) < 1+nonceSize+b.aead.Overhead() || envelope[0] != Version {
		return nil, ErrInvalidEnvelope
	}
	nonce := envelope[1 : 1+nonceSize]
	plain, err := b.aead.Open(nil, nonce, envelope[1+nonceSize:], aad(id, provider))
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return plain, nil
}

func aad(id, provider string) []byte { return []byte(id + "||" + provider) }

// MasterKeyFromEnv decodifica KEYPOOL_MASTER_KEY como base64 padrao ou raw-base64.
func MasterKeyFromEnv(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("cryptobox: KEYPOOL_MASTER_KEY ausente")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(value); err == nil {
			if len(b) != 32 {
				return nil, ErrInvalidKey
			}
			return b, nil
		}
	}
	return nil, errors.New("cryptobox: KEYPOOL_MASTER_KEY deve ser base64 de 32 bytes")
}

// MasterKeyCheck gera um envelope verificavel para detectar master key incorreta.
func (b *Box) MasterKeyCheck() ([]byte, error) {
	return b.Encrypt("keypool-master-check", "keypool", []byte("keypool master key check v1"))
}

// VerifyMasterKey verifica o envelope persistido em app_settings.master_key_check.
func (b *Box) VerifyMasterKey(check []byte) error {
	plain, err := b.Decrypt("keypool-master-check", "keypool", check)
	if err != nil {
		return fmt.Errorf("cryptobox: master key incorreta: %w", err)
	}
	sum := sha256.Sum256(plain)
	expected := sha256.Sum256([]byte("keypool master key check v1"))
	if sum != expected {
		return ErrInvalidEnvelope
	}
	return nil
}

// LoadOrCreateMasterKey prioriza a variavel de ambiente; caso ausente, carrega
// o arquivo base64. Um arquivo existente nunca e sobrescrito.
func LoadOrCreateMasterKey(path, envValue string) ([]byte, error) {
	if strings.TrimSpace(envValue) != "" {
		return MasterKeyFromEnv(envValue)
	}
	if path == "" {
		return nil, errors.New("cryptobox: caminho da master key ausente")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if runtime.GOOS != "windows" {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return nil, fmt.Errorf("cryptobox: verificar permissoes da master key: %w", statErr)
			}
			if info.Mode().Perm()&0077 != 0 {
				return nil, fmt.Errorf("cryptobox: permissoes inseguras na master key %s: %04o (esperado 0600)", path, info.Mode().Perm())
			}
		}
		return MasterKeyFromEnv(string(data))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cryptobox: ler master key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key) + "\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			if runtime.GOOS != "windows" {
				info, statErr := os.Stat(path)
				if statErr != nil {
					return nil, fmt.Errorf("cryptobox: verificar permissoes da master key: %w", statErr)
				}
				if info.Mode().Perm()&0077 != 0 {
					return nil, fmt.Errorf("cryptobox: permissoes inseguras na master key %s: %04o (esperado 0600)", path, info.Mode().Perm())
				}
			}
			return MasterKeyFromEnv(string(data))
		}
		return nil, fmt.Errorf("cryptobox: criar master key: %w", err)
	}
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}
