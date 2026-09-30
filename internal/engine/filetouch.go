package engine

import "github.com/liuzhixin405/cove-agent/internal/api"

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
	// The file key through every alias the file tools accept (the first one
	// set), then "path" (grep's and glob's root, or a file tool's path).
	// Reading only "filePath" and "path" left a read sent with file_path out
	// of Layer 3's file activity, so exploring new files counted as a stall.
	for _, key := range []string{"filePath", "file_path", "filepath", "file"} {
		if v, ok := tc.Input[key].(string); ok && v != "" {
			paths = append(paths, v)
			break
		}
	}
	if v, ok := tc.Input["path"].(string); ok && v != "" {
		paths = append(paths, v)
	}
	return paths
}
