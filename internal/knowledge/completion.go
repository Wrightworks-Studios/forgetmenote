package knowledge

// CompletionInput completes a workflow step: the consumer, the checkpoint
// it replaces, the revision it saw and its summary.
type CompletionInput struct {
	Consumer string `json:"consumer"`
	Previous string `json:"previous"`
	Revision string `json:"revision"`
	Summary  string `json:"summary"`
	Reset    bool   `json:"reset"`
}
