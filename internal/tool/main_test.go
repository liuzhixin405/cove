package tool

import (
	"os"
	"testing"
)

// TestMain fixes the go toolchain settings the LSP diagnostics tests need
// (see writeGoModule) for the whole package: set per test with t.Setenv they
// kept those slow tests from running in parallel. Only those tests run go.
func TestMain(m *testing.M) {
	os.Setenv("GOWORK", "off")
	os.Setenv("GOTOOLCHAIN", "local")
	os.Setenv("GOFLAGS", "-mod=mod")
	os.Exit(m.Run())
}
