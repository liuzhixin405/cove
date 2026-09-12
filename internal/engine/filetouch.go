package engine

import "github.com/liuzhixin405/cove/internal/api"

// touchPathsFor returns the filesystem paths a tool call gives the model new
// information about.
//
// Layer 3 uses these to tell real progress from a stall. Opening a file the
// model has never seen before is progress even when nothing is written, so
// read-only exploration, code walkthroughs and research are not reported as an
// empty run.
//
// Only paths the tool names explicitly are returned. bash and powershell expose
// their file operands through the command string instead; trackFileChanges
// feeds those in separately.
func touchPathsFor(tc api.ToolCall) []string {
	if len(tc.Input) == 0 {
		return nil
	}
	var paths []string
	for _, key := range []string{"filePath", "path"} {
		if v, ok := tc.Input[key].(string); ok && v != "" {
			paths = append(paths, v)
		}
	}
	return paths
}
