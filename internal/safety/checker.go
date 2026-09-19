package safety

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity represents the severity level of a safety finding.
type Severity int

const (
	SevInfo Severity = iota
	SevWarning
	SevError
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevWarning:
		return "warning"
	case SevError:
		return "error"
	case SevCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Finding represents a single safety issue detected.
type Finding struct {
	Rule     string // rule name that triggered
	Severity Severity
	Message  string
	Location string // tool name, file path, or "user_input"
}

// Result aggregates all findings from a safety scan.
type Result struct {
	Findings []Finding
	Passed   bool
}

// WorstSeverity returns the highest severity among all findings.
func (r *Result) WorstSeverity() Severity {
	worst := SevInfo
	for _, f := range r.Findings {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// BlockingFinding returns the first finding at Error or Critical severity.
func (r *Result) BlockingFinding() *Finding {
	for i := range r.Findings {
		if r.Findings[i].Severity >= SevError {
			return &r.Findings[i]
		}
	}
	return nil
}

// Check is a single safety rule.
type Check interface {
	Name() string
	Severity() Severity
	Check(input string) *Finding
}

// Checker runs a set of safety checks against input.
type Checker struct {
	checks []Check
}

// New creates a new Checker with the default security checks.
func New() *Checker {
	c := &Checker{}
	// Default checks
	c.AddCheck(&injectionCheck{})
	c.AddCheck(&secretCheck{})
	c.AddCheck(&dangerousCommandCheck{})
	return c
}

// AddCheck registers an additional safety check.
func (c *Checker) AddCheck(ch Check) {
	c.checks = append(c.checks, ch)
}

// Scan runs all registered checks against the input.
func (c *Checker) Scan(input string, location string) *Result {
	result := &Result{Passed: true, Findings: make([]Finding, 0)}
	for _, ch := range c.checks {
		if f := ch.Check(input); f != nil {
			f.Location = location
			result.Findings = append(result.Findings, *f)
			if f.Severity >= SevError {
				result.Passed = false
			}
		}
	}
	return result
}

// ScanToolCall checks a tool invocation for safety issues.
func (c *Checker) ScanToolCall(toolName string, params map[string]any) *Result {
	var input strings.Builder
	input.WriteString(toolName)
	for k, v := range params {
		fmt.Fprintf(&input, " %s=%v", k, v)
	}
	return c.Scan(input.String(), toolName)
}

// ──── Built-in Checks ────

// injectionCheck detects prompt injection patterns.
type injectionCheck struct{}

func (ch *injectionCheck) Name() string       { return "injection" }
func (ch *injectionCheck) Severity() Severity { return SevError }

var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all |previous )?instructions`),
	regexp.MustCompile(`(?i)forget (everything|your training)`),
	regexp.MustCompile(`(?i)you are now (DAN|STAN|a different)`),
	regexp.MustCompile(`(?i)system:\s*you are`),
	regexp.MustCompile(`(?i)<\|im_start\|>`),
}

func (ch *injectionCheck) Check(input string) *Finding {
	for _, pat := range injectionPatterns {
		if pat.MatchString(input) {
			return &Finding{
				Rule:     "injection",
				Severity: SevError,
				Message:  fmt.Sprintf("possible prompt injection detected: %s", pat.String()),
			}
		}
	}
	return nil
}

// secretCheck detects hardcoded secrets (API keys, tokens).
type secretCheck struct{}

func (ch *secretCheck) Name() string       { return "secrets" }
func (ch *secretCheck) Severity() Severity { return SevCritical }

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret|token|password|credential)\s*[:=]\s*['"]?[a-zA-Z0-9_\-]{20,}['"]?`),
	regexp.MustCompile(`sk-[a-zA-Z0-9]{32,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`),
	regexp.MustCompile(`github_pat_[a-zA-Z0-9_]{36,}`),
}

func (ch *secretCheck) Check(input string) *Finding {
	for _, pat := range secretPatterns {
		if m := pat.FindString(input); m != "" {
			return &Finding{
				Rule:     "secrets",
				Severity: SevCritical,
				Message:  fmt.Sprintf("hardcoded secret detected (masked): %s", maskSecret(m)),
			}
		}
	}
	return nil
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// dangerousCommandCheck detects potentially destructive shell commands.
type dangerousCommandCheck struct{}

func (ch *dangerousCommandCheck) Name() string { return "dangerous_command" }

// Severity reports the worst severity this check can produce; the per-finding
// severity is set in Check (SevCritical for catastrophic, SevWarning for risky).
func (ch *dangerousCommandCheck) Severity() Severity { return SevCritical }

// catastrophicCommands are irreversible and destroy state outside the project:
// the whole filesystem, the user's home directory, a raw device, the machine's
// permission model. These are reported at SevCritical so the engine's
// BlockingFinding check actually stops them.
//
// Every entry here used to be SevWarning, which the engine only logs — so a
// checker whose entire purpose was to stop `rm -rf /` never stopped anything.
var catastrophicCommands = []string{
	"rm -rf /", "rm -rf ~", "rm -fr /", "rm -fr ~",
	"rm -rf --no-preserve-root", "rm --no-preserve-root",
	":(){ :|:& };:", "fork bomb",
	"dd if=/dev/zero", "dd if=/dev/random", "mkfs.",
	"> /dev/sda", "> /dev/sdb", "> /dev/nvme",
	"chmod -r 777 /", "chmod 777 /", "chown -r / ",
	"format c:", "del /f /s /q c:\\",
}

// riskyCommands are destructive but project-scoped and routinely legitimate.
// They stay at SevWarning: surfaced to the user, not blocked.
var riskyCommands = []string{
	"git push --force origin", "git push -f origin",
	"git reset --hard", "git clean -fdx",
}

// riskyPatterns covers the risky cases a plain substring cannot express.
//
// "rm -rf ." as a substring matches "rm -rf ./node_modules/.cache" — an
// entirely routine command — so wiping the working directory has to be matched
// with "." as the whole argument.
var riskyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+-[rf]{2}\s+\.(\s|$)`),
	regexp.MustCompile(`(?i)\brm\s+-[rf]{2}\s+\./(\s|$)`),
	regexp.MustCompile(`(?i)\brm\s+-[rf]{2}\s+\*(\s|$)`),
}

func (ch *dangerousCommandCheck) Check(input string) *Finding {
	lower := strings.ToLower(input)
	// Catastrophic patterns are checked first: "rm -rf /" must not be reported
	// as the milder "rm -rf ." match when a command contains both.
	for _, cmd := range catastrophicCommands {
		if strings.Contains(lower, strings.ToLower(cmd)) {
			return &Finding{
				Rule:     "dangerous_command",
				Severity: SevCritical,
				Message:  fmt.Sprintf("irreversible destructive command refused: %s", cmd),
			}
		}
	}
	for _, cmd := range riskyCommands {
		if strings.Contains(lower, strings.ToLower(cmd)) {
			return &Finding{
				Rule:     "dangerous_command",
				Severity: SevWarning,
				Message:  fmt.Sprintf("potentially dangerous command: %s", cmd),
			}
		}
	}
	for _, pat := range riskyPatterns {
		if m := pat.FindString(input); m != "" {
			return &Finding{
				Rule:     "dangerous_command",
				Severity: SevWarning,
				Message:  fmt.Sprintf("potentially dangerous command: %s", strings.TrimSpace(m)),
			}
		}
	}
	return nil
}
