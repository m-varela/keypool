package cryptobox

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LoadDPAPIMasterKey abre uma master key protegida pelo DPAPI do usuario atual.
// O arquivo nao pode ser movido para outro usuario/maquina e continua exigindo backup seguro.
func LoadDPAPIMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("cryptobox: caminho DPAPI ausente")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: ler master key DPAPI: %w", err)
	}
	plain, err := unprotectMasterKey(data)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: descriptografar master key DPAPI: %w", err)
	}
	if len(plain) != 32 {
		return nil, ErrInvalidKey
	}
	return plain, nil
}

// LoadOrCreateDPAPIMasterKey cria um arquivo DPAPI sem substituir arquivos existentes.
func LoadOrCreateDPAPIMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("cryptobox: caminho DPAPI ausente")
	}
	if _, err := os.Stat(path); err == nil {
		return LoadDPAPIMasterKey(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	protected, err := protectMasterKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadDPAPIMasterKey(path)
		}
		return nil, err
	}
	if _, err = f.Write(protected); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}
