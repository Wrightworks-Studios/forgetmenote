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
