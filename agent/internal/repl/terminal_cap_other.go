//go:build !windows

package repl

import (
	"os"
	"strings"
)

func shouldUseFallbackReadline() bool {
	return forcePlainReadline()
}

func forcePlainReadline() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("AGENTGO_PLAIN_REPL")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
