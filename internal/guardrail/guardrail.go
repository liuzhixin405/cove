package guardrail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Action represents the guardrail decision.
type Action int

const (
	Allow Action = iota // proceed normally
	Warn                // proceed but inject a warning
	Block               // reject the tool call
)

// Decision is the guardrail result for a tool call.
type Decision struct {
	Action  Action
	Message string
}

// Rapid-failure circuit-breaker thresholds, counted per tool name over a
// 30-second window (a global count let five failed bash commands block the
// next read).
//
// These are named constants because the previous inline numbers had drifted
// from the documented behavior: the comment promised a block at 3 failures
// while the code blocked at 6, and the 4-failure branch was an empty `else if`
// that produced no warning at all — so the only signal the user ever got was
// the block, twice as late as intended.
const (
	rapidFailWarn  = 3
	rapidFailBlock = 5
)

// signature uniquely identifies a tool call by name + args hash.
type signature struct {
	Name     string
	ArgsHash string
}

// Tracker detects repetitive/looping tool calls and prevents infinite loops.
type Tracker struct {
	mu              sync.Mutex
	exactFailures   map[signature]int    // same tool+args failed repeatedly
	sameToolFails   map[string]int       // same tool name failed repeatedly
	idempotentSeen  map[signature]string // hash of last result for idempotent tools
	idempotentCount map[signature]int    // count of identical results
	recentFailures  []failureRecord      // sliding time-window for rapid-failure detection
}

type failureRecord struct {
	time     time.Time
	toolName string
	isError  bool
}

// New creates a new guardrail tracker.
func New() *Tracker {
	return &Tracker{
		exactFailures:   make(map[signature]int),
		sameToolFails:   make(map[string]int),
		idempotentSeen:  make(map[signature]string),
		idempotentCount: make(map[signature]int),
	}
}

// idempotent tools (read-only, no side effects)
var idempotentTools = map[string]bool{
	"read": true, "grep": true, "glob": true, "webfetch": true,
}

func makeSignature(name string, args map[string]any) signature {
	data, _ := json.Marshal(args)
	h := sha256.Sum256(data)
	return signature{Name: name, ArgsHash: hex.EncodeToString(h[:8])}
}

func hashResult(result string) string {
	h := sha256.Sum256([]byte(result))
	return hex.EncodeToString(h[:8])
}

// BeforeCall checks if a tool call should proceed.
func (t *Tracker) BeforeCall(name string, args map[string]any) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Rapid-failure circuit breaker: rapidFailBlock failures of this tool in
	// 30s → block, rapidFailWarn → warn.
	now := time.Now()
	window := 30 * time.Second
	cutoff := now.Add(-window)
	var recentFails int
	for _, f := range t.recentFailures {
		if f.time.After(cutoff) && f.isError && f.toolName == name {
			recentFails++
		}
	}
	// Trim old records
	var kept []failureRecord
	for _, f := range t.recentFailures {
		if f.time.After(cutoff) {
			kept = append(kept, f)
		}
	}
	t.recentFailures = kept

	if recentFails >= rapidFailBlock {
		return Decision{Action: Block, Message: fmt.Sprintf(
			"%s 短时间内连续失败 %d 次，已触发熔断。请暂停并检查根本原因。", name, recentFails)}
	}
	if recentFails >= rapidFailWarn {
		return Decision{Action: Warn, Message: fmt.Sprintf(
			"%s 最近 30 秒内已失败 %d 次，接近熔断阈值 %d，建议改变思路。", name, recentFails, rapidFailBlock)}
	}

	sig := makeSignature(name, args)

	// Check exact failure count
	if count := t.exactFailures[sig]; count >= 5 {
		return Decision{Action: Block, Message: "相同调用已连续失败 5 次，请换一种方法。"}
	} else if count >= 2 {
		return Decision{Action: Warn, Message: "此调用已连续失败多次，考虑换一种方式。"}
	}

	// Check same tool failure count
	if count := t.sameToolFails[name]; count >= 8 {
		return Decision{Action: Block, Message: "该工具已连续失败 8 次，请尝试其他工具或方法。"}
	} else if count >= 3 {
		return Decision{Action: Warn, Message: "该工具连续失败中，请注意是否需要调整。"}
	}

	// Check idempotent no-progress
	if idempotentTools[name] {
		if count := t.idempotentCount[sig]; count >= 5 {
			return Decision{Action: Block, Message: "只读工具返回相同结果 5 次，无需重复调用。"}
		} else if count >= 2 {
			return Decision{Action: Warn, Message: "该调用返回相同结果，重复调用无意义。"}
		}
	}

	return Decision{Action: Allow}
}

// AfterCall records the result of a tool call for future detection.
func (t *Tracker) AfterCall(name string, args map[string]any, result string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Record in sliding window for rapid-failure detection (consecutive failures only)
	if isError {
		t.recentFailures = append(t.recentFailures, failureRecord{
			time:     time.Now(),
			toolName: name,
			isError:  true,
		})
	} else {
		// Success resets this tool's rapid-failure window (only its own:
		// a successful read says nothing about the failing bash command).
		kept := t.recentFailures[:0]
		for _, f := range t.recentFailures {
			if f.toolName != name {
				kept = append(kept, f)
			}
		}
		t.recentFailures = kept
	}

	// Keep window bounded
	if len(t.recentFailures) > 100 {
		t.recentFailures = t.recentFailures[len(t.recentFailures)-100:]
	}

	sig := makeSignature(name, args)

	if isError {
		t.exactFailures[sig]++
		t.sameToolFails[name]++
	} else {
		// Success resets failure counters
		delete(t.exactFailures, sig)
		t.sameToolFails[name] = 0

		// Track idempotent results
		if idempotentTools[name] {
			rh := hashResult(result)
			if prev, ok := t.idempotentSeen[sig]; ok && prev == rh {
				t.idempotentCount[sig]++
			} else {
				t.idempotentSeen[sig] = rh
				t.idempotentCount[sig] = 0
			}
		}
	}
}

// Reset clears all tracking state (e.g., on new user turn).
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exactFailures = make(map[signature]int)
	t.sameToolFails = make(map[string]int)
	t.idempotentSeen = make(map[signature]string)
	t.idempotentCount = make(map[signature]int)
	// Used to be left out, so failures just before a reset still tripped the
	// breaker on the first call after it.
	t.recentFailures = nil
}
