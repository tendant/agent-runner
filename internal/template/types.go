package template

// TemplateMeta holds frontmatter metadata parsed from a template file.
type TemplateMeta struct {
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	ReadWhen string `json:"read_when"` // always | boot | first_run | heartbeat
	Priority int    `json:"priority"`
}

// TemplateFile represents a loaded template with metadata and body content.
type TemplateFile struct {
	Name string       // filename without path (e.g. "SOUL.md")
	Meta TemplateMeta // parsed frontmatter
	Body string       // content after frontmatter
}

// TemplateContext holds variables available for substitution in templates.
type TemplateContext struct {
	Message    string
	Repos      string
	Date       string
	Iteration  int
	ProjectDir string // absolute path to project root
}

// Phase represents the current execution phase for filtering templates.
type Phase string

const (
	PhaseBoot      Phase = "boot"
	PhaseHeartbeat Phase = "heartbeat"
)

// Well-known template filenames and their default priorities.
var wellKnownDefaults = map[string]struct {
	Priority int
	ReadWhen string
}{
	"IDENTITY.md":  {Priority: 10, ReadWhen: "always"},
	"SOUL.md":      {Priority: 20, ReadWhen: "always"},
	"AGENTS.md":    {Priority: 30, ReadWhen: "always"},
	"USER.md":      {Priority: 40, ReadWhen: "always"},
	"TOOLS.md":     {Priority: 50, ReadWhen: "always"},
	"BOOT.md":      {Priority: 70, ReadWhen: "boot"},
	"BOOTSTRAP.md": {Priority: 80, ReadWhen: "first_run"},
	"HEARTBEAT.md": {Priority: 90, ReadWhen: "heartbeat"},
}
