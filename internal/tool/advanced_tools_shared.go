package tool

import "github.com/liuzhixin405/cove/internal/textutil"

func ensureRuntimeMaps(rt *Runtime) {
	if rt.Tasks == nil {
		rt.Tasks = make(map[string]*TaskRecord)
	}
	if rt.Teams == nil {
		rt.Teams = make(map[string]*TeamRecord)
	}
	if rt.CronSchedules == nil {
		rt.CronSchedules = make(map[string]*CronRecord)
	}
}

// truncateStr limits s to at most n runes. The former s[:n-3] both split
// multi-byte runes and panicked outright for n < 3.
func truncateStr(s string, n int) string {
	return textutil.ClipRunes(s, n)
}
