package workspace

type SourceEvidence struct {
	Path        string `json:"path,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Heading     string `json:"heading,omitempty"`
	Hash        string `json:"hash,omitempty"`
	URL         string `json:"url,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}
type Finding struct {
	Key            string           `json:"key"`
	Path           string           `json:"path"`
	Title          string           `json:"title"`
	Origin         string           `json:"origin"`
	SourcePath     string           `json:"sourcePath,omitempty"`
	SourceRevision string           `json:"sourceRevision"`
	NoteHash       string           `json:"noteHash"`
	Evidence       []SourceEvidence `json:"evidence"`
	RequestHash    string           `json:"requestHash"`
	Dismissed      bool             `json:"dismissed,omitempty"`
	Stale          bool             `json:"stale"`
}
type FindingInput struct {
	Key      string           `json:"key"`
	Path     string           `json:"path"`
	Title    string           `json:"title"`
	Text     string           `json:"text"`
	Revision string           `json:"revision"`
	Hash     string           `json:"hash"`
	Evidence []SourceEvidence `json:"evidence"`
}
type ReviewFinding struct {
	Path     string  `json:"path"`
	Comment  Comment `json:"comment"`
	Revision string  `json:"revision"`
}
