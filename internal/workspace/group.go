package workspace

type NoteEdit struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Revision string `json:"revision"`
	Hash     string `json:"hash"`
	Delete   bool   `json:"delete,omitempty"`
}
type EditGroup struct {
	ID      string     `json:"id"`
	Summary string     `json:"summary"`
	Edits   []NoteEdit `json:"edits"`
}
type GroupResult struct {
	Revision string `json:"revision"`
	Replayed bool   `json:"replayed"`
}

type operationKey struct{}
type operationIdentity struct{ ID, Request string }
