package plugin

import "github.com/liuzhixin405/cove/internal/safepath"

// maxPluginNameLen is re-exported for tests that assert the boundary.
const maxPluginNameLen = safepath.MaxNameLen

// ValidatePluginName rejects any name that must not be used as a directory
// name under the plugins root. See internal/safepath for why this matters:
// plugin names reach the install paths from a cloned remote marketplace
// repository and are joined into a directory that is cloned into and
// os.RemoveAll'd on failure.
func ValidatePluginName(name string) error {
	return safepath.ValidateName("plugin", name)
}

// pluginDirFor resolves the on-disk directory for a plugin, validating the
// name first. Every path built from a plugin name must go through this.
func pluginDirFor(root, name string) (string, error) {
	return safepath.Join("plugin", root, name)
}
