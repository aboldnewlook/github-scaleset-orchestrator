package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const keyringService = "gso"

// Resolve returns the token from the first available source.
// Resolution order: Env → Keychain → File.
func (ts *TokenSource) Resolve() (string, error) {
	if ts == nil {
		return "", nil
	}

	// 1. Environment variable
	if ts.Env != "" {
		if val := os.Getenv(ts.Env); val != "" {
			return val, nil
		}
	}

	// 2. OS keychain
	if ts.Keychain != "" {
		token, err := keyring.Get(keyringService, ts.Keychain)
		if err == nil && token != "" {
			return token, nil
		}
		if err != nil && err != keyring.ErrNotFound {
			return "", fmt.Errorf("keychain lookup %q: %w", ts.Keychain, err)
		}
	}

	// 3. File
	if ts.File != "" {
		data, err := os.ReadFile(ts.File)
		if err != nil {
			return "", fmt.Errorf("reading token file %q: %w", ts.File, err)
		}
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	return "", nil
}

// StoreInKeychain saves a token to the OS keychain.
func StoreInKeychain(account, token string) error {
	return keyring.Set(keyringService, account, token)
}

// DeleteFromKeychain removes a token from the OS keychain.
func DeleteFromKeychain(account string) error {
	return keyring.Delete(keyringService, account)
}
