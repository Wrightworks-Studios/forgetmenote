package knowledge

// Processing retains recovery requirements and learned worker capacity, not
// notebook content. An engine owns its mutable fields until it has been drained.
// A registry may then reuse it while reopening the same notebook's heavy index.
type Processing struct {
	currentVectorPersistFailure string
	failure                     string
	syncFailure                 string
	historyVectorPersistFailure string
	historyPersistFailure       string
	historyResume               bool
	historyFailure              string
	historyOwnerFailure         string
	historySyncFailure          string
	historyEmbeddingFailure     string
	historySubmissionFailure    string
	batchLimit                  int
	embeddingFailed             bool
	notify                      func(bool)
}

func NewProcessing(notify func(bool)) *Processing { return &Processing{notify: notify} }
