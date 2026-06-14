package tool

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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
