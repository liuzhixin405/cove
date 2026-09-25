package dream

import (
	"os"
	"strings"
	"testing"
	"time"
)

// min_interval_minutes holds session-end consolidations apart; 0 (the
// default) keeps the former behaviour.
func TestSessionEndDueHonoursMinInterval(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	if LoadConfig().MinIntervalMinutes != 0 {
		t.Fatal("min_interval_minutes default is not 0")
	}
	if err := RecordConsolidation(); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(lockPath(), past, past)
	touchSession(t, sessions, "cur")
	if due, why := SessionEndDue(3, sessions); !due {
		t.Fatalf("not due without an interval: %s", why)
	}
	writeDreamJSON(t, cfgDir, `{"min_interval_minutes": 60}`)
	due, why := SessionEndDue(3, sessions)
	if due || !strings.Contains(why, "min_interval_minutes") {
		t.Fatalf("due=%v why=%q 10 minutes after the last run with a 60-minute interval", due, why)
	}
	writeDreamJSON(t, cfgDir, `{"min_interval_minutes": 5}`)
	if due, why := SessionEndDue(3, sessions); !due {
		t.Fatalf("not due 10 minutes after the last run with a 5-minute interval: %s", why)
	}
}
