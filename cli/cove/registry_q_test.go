package main

import "testing"

// The repo map left the system prompt; repo_map is how the model queries it,
// so it is registered in every mode (headless included).
func TestRepoMapToolRegistered(t *testing.T) {
	for _, o := range []toolOptions{
		{goos: "linux"},
		{goos: "windows", interactive: true},
		{goos: "darwin", interactive: true, experimental: true, chrome: true},
	} {
		if !namesWith(o)["repo_map"] {
			t.Errorf("repo_map missing with %+v", o)
		}
	}
}
