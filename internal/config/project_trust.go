package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

// trustStoreName is the file in ConfigDir that records which project
// .cove.json files the user trusted: absolute path -> sha256 of the content
// that was trusted. Trust is tied to the content, so any edit to the file
// (a `git pull` that adds an MCP server) needs a new decision.
const trustStoreName = "trusted_projects.json"

// trustStoreMu serialises read-modify-write of the store within the process.
var trustStoreMu sync.Mutex

// UntrustedProjectConfig reports the project .cove.json whose sensitive
// fields Load ignored because the file is not trusted, and those fields' JSON
// names (e.g. "mcp_servers", "provider.base_url"). path is "" when nothing was
// ignored: no .cove.json, a trusted one, or one with only safe fields.
func (c *Config) UntrustedProjectConfig() (path string, fields []string) {
	if c == nil || len(c.untrustedFields) == 0 {
		return "", nil
	}
	return c.projectConfigPath, append([]string(nil), c.untrustedFields...)
}

// TrustLoadedProjectConfig trusts the .cove.json content Load read, which is
// what UntrustedProjectConfig described to the user. Unlike
// TrustProjectConfig it cannot trust a file that was changed after the user
// looked at it. Reload the config afterwards for the fields to apply.
func (c *Config) TrustLoadedProjectConfig() error {
	if c == nil || c.projectConfigPath == "" {
		return errors.New("没有已加载的项目配置 .cove.json")
	}
	if err := recordTrust(c.projectConfigPath, c.projectConfigHash); err != nil {
		return err
	}
	// Trusting the project's settings is a decision about the project: its
	// directory is trusted too, so automatic build/test verification runs.
	return TrustProjectDir(filepath.Dir(c.projectConfigPath))
}

// TrustProjectConfig records the current content of the .cove.json at path
// as trusted. Load then applies its sensitive fields until the file changes.
func TrustProjectConfig(path string) error {
	key, err := trustKey(path)
	if err != nil {
		return err
	}
	// Read as Load reads it, so a link or device is refused here too.
	data, err := readProjectConfigFile(key)
	if err != nil {
		return err
	}
	if err := recordTrust(key, contentHash(data)); err != nil {
		return err
	}
	return TrustProjectDir(filepath.Dir(key))
}

// dirTrustPrefix marks a directory entry in the trust store. File entries
// are keyed by the .cove.json path and hold a content hash; a directory has
// no content to hash, so it holds dirTrustValue under a prefixed key that no
// file path can collide with.
const (
	dirTrustPrefix = "dir:"
	dirTrustValue  = "trusted"
)

// TrustProjectDir records the project directory root as trusted: cove then
// runs the build and test commands it detects there (npm run build, cargo
// check, go test ...) at the end of a turn without asking. Those commands
// execute code from the repository, so a clone must not get them just by
// being opened.
func TrustProjectDir(root string) error {
	key, err := trustKey(root)
	if err != nil {
		return err
	}
	return recordTrust(dirTrustPrefix+key, dirTrustValue)
}

// IsProjectDirTrusted reports whether the directory root itself was trusted
// (TrustProjectDir, or trusting a .cove.json in it). Subdirectories are not
// implied: callers check the directory they run in and its project root.
func IsProjectDirTrusted(root string) (bool, error) {
	key, err := trustKey(root)
	if err != nil {
		return false, err
	}
	return isTrustedHash(dirTrustPrefix+key, dirTrustValue)
}

// IsProjectConfigTrusted reports whether the .cove.json at path is trusted
// with its current content. A missing file is not trusted and not an error.
func IsProjectConfigTrusted(path string) (bool, error) {
	key, err := trustKey(path)
	if err != nil {
		return false, err
	}
	data, err := readProjectConfigFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isTrustedHash(key, contentHash(data))
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// trustKey is the store key for path: absolute and cleaned, so "./.cove.json"
// and the full path are the same entry.
func trustKey(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// storeKey folds case on Windows, whose paths are case-insensitive: the same
// project reached as C:\Repo and c:\repo must not need trusting twice.
func storeKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func isTrustedHash(path, hash string) (bool, error) {
	store, err := readTrustStore()
	if err != nil {
		return false, err
	}
	return store[storeKey(path)] == hash, nil
}

func recordTrust(path, hash string) error {
	trustStoreMu.Lock()
	defer trustStoreMu.Unlock()
	store, err := readTrustStore()
	if err != nil {
		return err
	}
	store[storeKey(path)] = hash
	return writeTrustStore(store)
}

func trustStorePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, trustStoreName), nil
}

func readTrustStore() (map[string]string, error) {
	p, err := trustStorePath()
	if err != nil {
		return nil, err
	}
	store := map[string]string{}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if body := stripBOM(data); len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &store); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if store == nil {
			store = map[string]string{}
		}
	}
	return store, nil
}

func writeTrustStore(store map[string]string) error {
	p, err := trustStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(p, out, 0o600)
}
