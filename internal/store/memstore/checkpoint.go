package memstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"keypool/internal/clock"
	"keypool/internal/config"
	"keypool/internal/cryptobox"
	"keypool/internal/sched"
	"keypool/internal/secret"
	"keypool/internal/store"
)

const checkpointVersion = 1

var errNotInitialized = errors.New("memstore: estado ainda não inicializado")
var errAlreadyInitialized = errors.New("memstore: estado já inicializado")
var errClosed = errors.New("memstore: store fechado")

// Options configura a abertura do checkpoint local.
type Options struct {
	Dir   string
	Box   *cryptobox.Box
	Clock clock.Clock
}

type apiKeyCheckpoint struct {
	Hash      string    `json:"hash"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
	RotatedAt time.Time `json:"rotated_at,omitempty"`
}

type keyCheckpoint struct {
	Provider  string      `json:"provider"`
	ID        sched.KeyID `json:"id"`
	Label     string      `json:"label"`
	Group     string      `json:"group,omitempty"`
	Tags      []string    `json:"tags,omitempty"`
	Enabled   bool        `json:"enabled"`
	SecretEnc string      `json:"secret_enc"`
	secret    string      `json:"-"`
}

type checkpoint struct {
	Version        int                           `json:"version"`
	SavedAt        time.Time                     `json:"saved_at"`
	APIKey         apiKeyCheckpoint              `json:"api_key"`
	MasterKeyCheck string                        `json:"master_key_check"`
	Config         *config.Raw                   `json:"config"`
	Keys           []keyCheckpoint               `json:"keys,omitempty"`
	Pools          map[string]sched.PoolSnapshot `json:"pools,omitempty"`
	Daily          dailyState                    `json:"daily"`
}

// Open lê o checkpoint existente sem criar diretórios ou arquivos.
func Open(ctx context.Context, options Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Dir) == "" || options.Box == nil {
		return nil, errors.New("memstore: diretório e cryptobox são obrigatórios")
	}
	if options.Clock == nil {
		options.Clock = clock.Real()
	}
	s := newStore(options.Dir, options.Box, options.Clock)
	path := filepath.Join(options.Dir, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memstore: ler state.json: %w", err)
	}
	var saved checkpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&saved); err != nil {
		return nil, fmt.Errorf("memstore: state.json inválido: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("memstore: conteúdo adicional após state.json")
	}
	if saved.Version != checkpointVersion {
		return nil, fmt.Errorf("memstore: versão de state.json não suportada: %d", saved.Version)
	}
	check, err := base64.StdEncoding.DecodeString(saved.MasterKeyCheck)
	if err != nil {
		return nil, fmt.Errorf("memstore: master_key_check inválido: %w", err)
	}
	if err = options.Box.VerifyMasterKey(check); err != nil {
		return nil, fmt.Errorf("memstore: validar master key: %w", err)
	}
	if err = s.restoreCheckpoint(&saved); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) restoreCheckpoint(saved *checkpoint) error {
	if saved.Config == nil {
		return errors.New("memstore: state.json sem configuração")
	}
	raw, err := cloneRaw(saved.Config)
	if err != nil {
		return fmt.Errorf("memstore: decodificar configuração: %w", err)
	}
	keys := make([]store.KeyRecord, 0, len(saved.Keys))
	for _, item := range saved.Keys {
		envelope, decodeErr := base64.StdEncoding.DecodeString(item.SecretEnc)
		if decodeErr != nil {
			return fmt.Errorf("memstore: cifra inválida para chave %s/%s: %w", item.Provider, item.ID, decodeErr)
		}
		plain, decryptErr := s.box.Decrypt(string(item.ID), item.Provider, envelope)
		if decryptErr != nil {
			return fmt.Errorf("memstore: decifrar chave %s/%s: %w", item.Provider, item.ID, decryptErr)
		}
		keys = append(keys, store.KeyRecord{
			Provider: item.Provider, ID: item.ID, Label: item.Label, Group: item.Group,
			Tags: append([]string(nil), item.Tags...), Secret: secret.New(string(plain)), Enabled: item.Enabled,
		})
	}
	pools := make(map[string]sched.PoolSnapshot, len(saved.Pools))
	for name, state := range saved.Pools {
		cloned, cloneErr := clonePoolSnapshot(state)
		if cloneErr != nil {
			return fmt.Errorf("memstore: decodificar snapshot %s: %w", name, cloneErr)
		}
		pools[name] = cloned
	}
	hash, err := hex.DecodeString(saved.APIKey.Hash)
	if err != nil || len(hash) != sha256.Size {
		return errors.New("memstore: hash da API key inválido")
	}
	var keyHash [sha256.Size]byte
	copy(keyHash[:], hash)
	s.mu.Lock()
	s.raw = raw
	s.keys = keys
	s.pools = pools
	s.apiKey = saved.APIKey
	s.apiKeyHash = keyHash
	s.masterKeyCheck = saved.MasterKeyCheck
	s.initialized = true
	s.mu.Unlock()
	s.usageMu.Lock()
	s.daily = cloneDaily(saved.Daily)
	if s.daily.ProjectTokens == nil {
		s.daily.ProjectTokens = map[string]int{}
	}
	if s.daily.WarmL2Tokens == nil {
		s.daily.WarmL2Tokens = map[string]int{}
	}
	if s.daily.Day == "" {
		s.daily = newDaily(s.clock.Now())
	}
	s.dailyDirty = false
	s.dailyVersion = 0
	s.usageMu.Unlock()
	return nil
}

func (s *Store) snapshotCheckpoint() (*checkpoint, uint64, error) {
	s.mu.RLock()
	if !s.initialized {
		s.mu.RUnlock()
		return nil, 0, errNotInitialized
	}
	raw, err := cloneRaw(s.raw)
	if err != nil {
		s.mu.RUnlock()
		return nil, 0, err
	}
	keys := make([]keyCheckpoint, 0, len(s.keys))
	for _, key := range s.keys {
		keys = append(keys, keyCheckpoint{
			Provider: key.Provider, ID: key.ID, Label: key.Label, Group: key.Group,
			Tags: append([]string(nil), key.Tags...), Enabled: key.Enabled, secret: key.Secret.Reveal(),
		})
	}
	pools := make(map[string]sched.PoolSnapshot, len(s.pools))
	for name, state := range s.pools {
		cloned, cloneErr := clonePoolSnapshot(state)
		if cloneErr != nil {
			s.mu.RUnlock()
			return nil, 0, cloneErr
		}
		pools[name] = cloned
	}
	apiKey := s.apiKey
	masterCheck := s.masterKeyCheck
	s.mu.RUnlock()

	s.usageMu.Lock()
	daily := cloneDaily(s.daily)
	dailyVersion := s.dailyVersion
	s.usageMu.Unlock()

	return &checkpoint{
		Version: checkpointVersion, APIKey: apiKey, MasterKeyCheck: masterCheck,
		Config: raw, Keys: keys, Pools: pools, Daily: daily,
	}, dailyVersion, nil
}

func (s *Store) writeCheckpoint(ctx context.Context, saved *checkpoint) error {
	for i := range saved.Keys {
		ciphertext, err := s.box.Encrypt(string(saved.Keys[i].ID), saved.Keys[i].Provider, []byte(saved.Keys[i].secret))
		if err != nil {
			return fmt.Errorf("memstore: cifrar chave %s/%s: %w", saved.Keys[i].Provider, saved.Keys[i].ID, err)
		}
		saved.Keys[i].SecretEnc = base64.StdEncoding.EncodeToString(ciphertext)
	}
	saved.SavedAt = s.clock.Now().UTC()
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return fmt.Errorf("memstore: serializar state.json: %w", err)
	}
	return atomicWrite(ctx, s.dir, data, s.clock)
}

func atomicWrite(ctx context.Context, dir string, data []byte, clk clock.Clock) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("memstore: criar diretório de dados: %w", err)
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	destination := filepath.Join(dir, "state.json")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("memstore: criar state.json.tmp: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return fmt.Errorf("memstore: gravar state.json.tmp: %w", err)
	}
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err = ctx.Err(); err != nil {
			cleanup()
			return err
		}
		renameErr = os.Rename(tmp, destination)
		if renameErr == nil {
			break
		}
		if attempt < 4 {
			if waitErr := clk.Wait(ctx, time.Duration(attempt+1)*25*time.Millisecond, nil); waitErr != nil {
				cleanup()
				return waitErr
			}
		}
	}
	if renameErr != nil {
		cleanup()
		return fmt.Errorf("memstore: substituir state.json: %w", renameErr)
	}
	if runtime.GOOS != "windows" {
		if directory, openErr := os.Open(dir); openErr == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}
	return nil
}

func hashAPIKey(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func verifyAPIKey(hash [sha256.Size]byte, token string) bool {
	candidate := hashAPIKey(token)
	return subtle.ConstantTimeCompare(hash[:], candidate[:]) == 1
}

func base64String(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

func apiKeyPrefix(token string) string {
	if len(token) > 7 {
		return token[:7]
	}
	return token
}

func apiKeyState(token string, now time.Time, previous apiKeyCheckpoint) apiKeyCheckpoint {
	state := previous
	hash := hashAPIKey(token)
	state.Hash = hex.EncodeToString(hash[:])
	state.Prefix = apiKeyPrefix(token)
	if state.CreatedAt.IsZero() {
		state.CreatedAt = now.UTC()
	} else {
		state.RotatedAt = now.UTC()
	}
	return state
}
