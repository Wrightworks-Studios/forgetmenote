package notebooks

type DocumentRef struct {
	Project  string `json:"project"`
	Origin   string `json:"origin,omitempty" jsonschema:"Content origin: notes for editable notebook files; repository for connected source files. Defaults to the notebook authority."`
	Source   string `json:"source,omitempty"`
	NoteID   string `json:"noteId,omitempty"`
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty"`
	Heading  string `json:"heading,omitempty"`
}

// DocumentOutput is one read of a notebook or repository document: its
// reference, content, citation and the range read.
type DocumentOutput struct {
	DocumentRef
	Health    *RecipeHealth `json:"health,omitempty"`
	Content   string        `json:"content"`
	HTML      string        `json:"html,omitempty"`
	Citation  string        `json:"citation"`
	Hash      string        `json:"hash"`
	Editable  bool          `json:"editable"`
	StartByte int           `json:"startByte,omitempty"`
	EndByte   int           `json:"endByte,omitempty"`
	StartLine int           `json:"startLine,omitempty"`
	EndLine   int           `json:"endLine,omitempty"`
	Lines     int           `json:"lines,omitempty"`
	Cursor    string        `json:"cursor,omitempty"`
	More      bool          `json:"more,omitempty"`
}

// RecipeHealth is the state of an automation recipe's latest checks.
type RecipeHealth struct {
	State     string            `json:"state"`
	CheckedAt string            `json:"checkedAt,omitempty"`
	Checks    map[string]string `json:"checks,omitempty"`
}
