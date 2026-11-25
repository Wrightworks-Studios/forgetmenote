package retrieval

const (
	DefaultLimit       = 12
	DefaultPerFile     = 4
	DefaultTokenBudget = 6000
)

// Budget packs already ranked evidence. Ranking, duplicate detection and access
// checks belong to the caller; rejected evidence never consumes this budget.
type Budget struct {
	limit, tokenLimit, perFile int
	count, tokens              int
	files                      map[string]int
}

// A zero perFile allows callers that already applied diversity to keep it.
func NewBudget(limit, tokens, perFile int) *Budget {
	if limit == 0 {
		limit = DefaultLimit
	}
	if tokens == 0 {
		tokens = DefaultTokenBudget
	}
	return &Budget{limit: limit, tokenLimit: tokens, perFile: perFile, files: map[string]int{}}
}

func (b *Budget) Full(key string) bool {
	return b.count >= b.limit || b.perFile > 0 && b.files[key] >= b.perFile
}

func (b *Budget) Add(key string, tokens int) bool {
	if b.Full(key) || tokens > b.tokenLimit-b.tokens {
		return false
	}
	b.count++
	b.tokens += tokens
	b.files[key]++
	return true
}

func (b *Budget) Tokens() int { return b.tokens }

// EvidenceTokens estimates source/mixed evidence including citation metadata.
// Notebook-only retrieval estimates its own complete serialized passage instead.
func EvidenceTokens(path, text string) int { return (len(text) + len(path) + 600) / 3 }
