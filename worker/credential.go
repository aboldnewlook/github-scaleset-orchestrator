package worker

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// ErrNoCredential means no worker credential is stored: the worker must enroll.
var ErrNoCredential = errors.New("no worker credential stored")

// CredentialStore keeps the "<worker_id>.<secret>" credential. A keychain is
// not a boundary against other processes of the same user (design spec §8.2):
// job code is kept away from that user instead.
type CredentialStore interface {
	Load() (string, error) // ErrNoCredential when absent
	Save(credential string) error
	Delete() error
}

// FileCredentialStore keeps the credential in a file. Load refuses a file that
// group or others can access (non-Windows); Save writes it with mode 0600.
func FileCredentialStore(path string) CredentialStore {
	return fileCredentialStore{path: path}
}

type fileCredentialStore struct{ path string }

func (s fileCredentialStore) Load() (string, error) {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", fmt.Errorf("reading worker credential: %w", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf("worker credential file %s has mode %04o; it must not be readable by group or others (chmod 600 %s)", s.path, perm, s.path)
		}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("reading worker credential: %w", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		return "", ErrNoCredential
	}
	return credential, nil
}

func (s fileCredentialStore) Save(credential string) error {
	if err := writeFileAtomic(s.path, []byte(credential+"\n"), 0o600); err != nil {
		return fmt.Errorf("saving worker credential: %w", err)
	}
	return nil
}

func (s fileCredentialStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting worker credential: %w", err)
	}
	return nil
}

// KeychainCredentialStore keeps the credential in the OS keychain (macOS
// Keychain, Windows Credential Manager, Secret Service) under service/account.
func KeychainCredentialStore(service, account string) CredentialStore {
	return keychainCredentialStore{service: service, account: account}
}

type keychainCredentialStore struct{ service, account string }

func (s keychainCredentialStore) Load() (string, error) {
	credential, err := keyring.Get(s.service, s.account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", fmt.Errorf("reading worker credential from the keychain: %w", err)
	}
	if strings.TrimSpace(credential) == "" {
		return "", ErrNoCredential
	}
	return credential, nil
}

func (s keychainCredentialStore) Save(credential string) error {
	if err := keyring.Set(s.service, s.account, credential); err != nil {
		return fmt.Errorf("saving worker credential to the keychain: %w", err)
	}
	return nil
}

func (s keychainCredentialStore) Delete() error {
	if err := keyring.Delete(s.service, s.account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("deleting worker credential from the keychain: %w", err)
	}
	return nil
}
