package command

type CommitCmd struct{}
type ReviewCmd struct{}
type DoctorCmd struct{}
type ConfigCmd struct{}
type CompactCmd struct{}
type CostCmd struct{}
type DiffCmd struct{}
type MemoryCmd struct{}
type ResumeCmd struct{}
type McpCmd struct{}
type PluginCmd struct{}
type SkillsCmd struct{}
type ExportCmd struct{}
type SystemCmd struct{}
type DreamCmd struct{}

type CdCmd struct{}
type ContextCmd struct{}

type PermissionsCmd struct{}
type StatusCmd struct{}
type StatsCmd struct{}
type InitCmd struct{}

func NewCommitCmd() Command      { return &CommitCmd{} }
func NewReviewCmd() Command      { return &ReviewCmd{} }
func NewDoctorCmd() Command      { return &DoctorCmd{} }
func NewConfigCmd() Command      { return &ConfigCmd{} }
func NewCompactCmd() Command     { return &CompactCmd{} }
func NewCostCmd() Command        { return &CostCmd{} }
func NewDiffCmd() Command        { return &DiffCmd{} }
func NewMemoryCmd() Command      { return &MemoryCmd{} }
func NewResumeCmd() Command      { return &ResumeCmd{} }
func NewMcpCmd() Command         { return &McpCmd{} }
func NewPluginCmd() Command      { return &PluginCmd{} }
func NewSkillsCmd() Command      { return &SkillsCmd{} }
func NewExportCmd() Command      { return &ExportCmd{} }
func NewSystemCmd() Command      { return &SystemCmd{} }
func NewCdCmd() Command          { return &CdCmd{} }
func NewDreamCmd() Command       { return &DreamCmd{} }
func NewContextCmd() Command     { return &ContextCmd{} }
func NewPermissionsCmd() Command { return &PermissionsCmd{} }
func NewStatusCmd() Command      { return &StatusCmd{} }
func NewStatsCmd() Command       { return &StatsCmd{} }
func NewInitCmd() Command        { return &InitCmd{} }
