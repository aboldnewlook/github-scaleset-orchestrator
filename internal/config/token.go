package config

import (
	"fmt"
	"os"
	"runtime"
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
		if err := checkTokenFilePrivate(ts.File); err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	return "", nil
}

// checkTokenFilePrivate refuses a token file that group or others can access.
// Windows has no POSIX mode bits (Go reports 0666 or 0444 there), so the check
// is skipped on Windows; protect the file with an ACL instead.
func checkTokenFilePrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking token file %q: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("token file %q has permissions %04o; it must not be readable by group or others (chmod 600 %s)", path, perm, path)
	}
	return nil
}

// StoreInKeychain saves a token to the OS keychain.
func StoreInKeychain(account, token string) error {
	return keyring.Set(keyringService, account, token)
}

// DeleteFromKeychain removes a token from the OS keychain.
func DeleteFromKeychain(account string) error {
	return keyring.Delete(keyringService, account)
}
