// Package safepath validates untrusted strings that are about to become
// filesystem path elements.
//
// Several features let a name chosen elsewhere become a directory under a fixed
// root: plugins (`~/.cove/plugins/<name>`) and skills (`~/.cove/skills/<name>`).
// Those names are not trustworthy — besides what a user types into a slash
// command, they also arrive from manifests inside a cloned REMOTE marketplace
// repository and from a remote skills registry. Joined unchecked, a name like
// "../../../.ssh" redirects the directory creation, the git clone target, and
// the os.RemoveAll cleanup outside the intended root.
package safepath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// MaxNameLen bounds how long a single path element may be.
const MaxNameLen = 64

// ValidateName reports whether name is safe to use as a single path element
// under a fixed root.
//
// The allowed set is deliberately narrow — letters, digits, '-', '_', '.' — and
// is a strict superset of what real plugin and skill names look like. It leaves
// no room for separators, traversal, drive letters, UNC prefixes, NUL, shell
// metacharacters, or leading dots.
//
// kind names the thing being validated and appears in the error message.
func ValidateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", kind)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%s name too long (%d > %d): %q", kind, len(name), MaxNameLen, name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid %s name: %q", kind, name)
	}
	// A leading dot would hide the directory and can collide with bookkeeping
	// suffixes such as ".disabled".
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%s name may not start with a dot: %q", kind, name)
	}
	if strings.HasSuffix(name, ".disabled") {
		return fmt.Errorf("%s name may not end with .disabled: %q", kind, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s name contains an illegal character %q: %q", kind, r, name)
		}
	}
	// Belt and braces: whatever the character scan allowed, the name must still
	// be exactly one path element that Join cannot escape.
	if filepath.Base(name) != name || filepath.Clean(name) != name {
		return fmt.Errorf("invalid %s name: %q", kind, name)
	}
	return nil
}

// Join validates name and returns filepath.Join(root, name). The returned path
// is always directly under root.
func Join(kind, root, name string) (string, error) {
	if err := ValidateName(kind, name); err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}
