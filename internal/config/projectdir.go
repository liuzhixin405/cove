package config

import (
	"crypto/sha1" //nolint:gosec // non-cryptographic directory key
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProjectKey returns the 8-hex-char key used to namespace per-project data.
// The key is derived from sha1 over filepath.Clean(projectRoot), lower-cased
// on Windows (case-insensitive filesystem).
func ProjectKey(projectRoot string) string {
	p := filepath.Clean(projectRoot)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	sum := sha1.Sum([]byte(p)) //nolint:gosec // non-cryptographic directory key
	return hex.EncodeToString(sum[:])[:8]
}

// ProjectDataPath is ProjectDataDir without creating the directory.
func ProjectDataPath(projectRoot string) (string, error) {
	base, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "projects", ProjectKey(projectRoot)), nil
}

// ProjectDataDir returns <ConfigDir()>/projects/<ProjectKey(projectRoot)> and
// creates the directory if needed.
func ProjectDataDir(projectRoot string) (string, error) {
	dir, err := ProjectDataPath(projectRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
