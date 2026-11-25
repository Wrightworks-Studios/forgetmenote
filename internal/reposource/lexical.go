package reposource

import (
	"context"
	"errors"
	"forgetmenote/internal/retrieval"
	"path"
	"regexp"
	"strings"
	"unicode"
)

const lexicalAllowance = 256 << 20

// A single admitted snapshot bounds instance-wide source lexical memory. Other
// repositories are searched sequentially; switching evicts the previous entry.
type lexicalCache struct {
	key   string
	index sourceIndex
	terms *sourceLexicon
}
type sourceLexicon struct{ terms *retrieval.Lexicon[uint16] }

// The caller holds the notebook lock and indexMu. Read fresh generation metadata
// for vector files, but share immutable chunks and terms across search and indexing.
func (m *Manager) lexicalIndex(ctx context.Context, c *cache, current State) (sourceIndex, error) {
	idx, err := c.loadIndexMetadata(ctx, current.Index.Generation)
	if err != nil {
		return idx, err
	}
	key := current.Project + "/" + current.ID + "/" + idx.Commit + "/" + idx.ConfigID + "/" + idx.ChunkHash + "/" + idx.MapVersion
	if m.lexical != nil && m.lexical.key == key {
		idx.Chunks = m.lexical.index.Chunks
		if idx.MapHash == m.lexical.index.MapHash {
			idx.Map = m.lexical.index.Map
		} else {
			if err := idx.loadMap(); err != nil {
				return idx, err
			}
			m.lexical.index.Map, m.lexical.index.MapHash = idx.Map, idx.MapHash
		}
		return idx, nil
	}
	m.lexical = nil
	idx, err = c.loadIndex(ctx, current.Index.Generation)
	if err != nil {
		return idx, err
	}
	lex, err := newSourceLexicon(ctx, idx)
	if err == nil {
		m.lexical = &lexicalCache{key: key, index: idx, terms: lex}
	}
	return idx, err
}

func newSourceLexicon(ctx context.Context, idx sourceIndex) (*sourceLexicon, error) {
	navigation := map[string]string{}
	for _, entry := range idx.Map {
		navigation[entry.Path] = mapTopics[entry.Role]
	}
	terms, err := retrieval.NewLexicon[uint16](ctx, len(idx.Chunks), func(i int) [4]string {
		ch := idx.Chunks[i]
		return [4]string{ch.Text, ch.Symbol, ch.Path, navigation[ch.Path]}
	}, retrieval.SourceTokens, lexicalAllowance)
	if errors.Is(err, retrieval.ErrLexiconLimit) {
		return nil, ErrLimit
	}
	if err != nil {
		return nil, err
	}
	return &sourceLexicon{terms: terms}, nil
}

func (l *sourceLexicon) scores(ctx context.Context, idx sourceIndex, q SearchQuery) (map[int]float64, map[int]bool, error) {
	terms := []string{}
	seen := map[string]bool{}
	for _, term := range retrieval.SourceTokens(q.Query) {
		if seen[term] || retrieval.Framing(term) || len(seen) >= 64 {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	scores, err := l.terms.Scores(ctx, terms, [4]float64{1, 3, 2, 1}, func(doc int, _ string) bool { return strings.HasPrefix(idx.Chunks[doc].Path, q.PathPrefix) })
	if err != nil {
		return nil, nil, err
	}
	exact := map[int]bool{}
	query := strings.ToLower(strings.Trim(strings.TrimSpace(q.Query), "\"`"))
	// Identifier-shaped terms inside a question protect their definitions from
	// semantic dilution. Incidental mentions remain ordinary lexical candidates.
	for _, term := range queryIdentifiers(q.Query) {
		for _, p := range l.terms.Postings(term) {
			i := int(p.Document)
			if strings.HasPrefix(idx.Chunks[i].Path, q.PathPrefix) && p.Frequency[1] > 0 {
				exact[i] = true
				scores[i] = max(scores[i], .001)
			}
		}
		for i, ch := range idx.Chunks {
			if strings.HasPrefix(ch.Path, q.PathPrefix) && (strings.EqualFold(ch.Path, term) || strings.EqualFold(path.Base(ch.Path), term)) {
				exact[i] = true
				scores[i] = max(scores[i], .001)
			}
		}
	}
	// Full identifiers remain exact matches even when camel/snake components were
	// also indexed. Sentence fragments do not acquire exact-match priority.
	if !strings.ContainsAny(query, " \t\n") {
		for _, p := range l.terms.Postings(query) {
			i := int(p.Document)
			if strings.HasPrefix(idx.Chunks[i].Path, q.PathPrefix) && (p.Frequency[0] > 0 || p.Frequency[1] > 0 || p.Frequency[2] > 0) {
				exact[i] = true
				scores[i] = max(scores[i], .001)
			}
		}
	}
	for i, ch := range idx.Chunks {
		if strings.HasPrefix(ch.Path, q.PathPrefix) && (strings.EqualFold(ch.Path, query) || strings.EqualFold(path.Base(ch.Path), query) || ch.Symbol != "" && strings.EqualFold(ch.Symbol, query)) {
			exact[i] = true
			scores[i] = max(scores[i], .001)
		}
	}
	return scores, exact, nil
}

var queryName = regexp.MustCompile(`[\pL_][\pL\pN_./-]*`)

func queryIdentifiers(query string) []string {
	terms := []string{}
	for _, word := range queryName.FindAllString(query, 64) {
		word = strings.TrimRight(word, ".")
		identifier := strings.ContainsAny(word, "_/.")
		var previous rune
		for _, r := range word {
			if unicode.IsLower(previous) && unicode.IsUpper(r) {
				identifier = true
			}
			previous = r
		}
		if identifier {
			terms = append(terms, strings.ToLower(word))
		}
	}
	return terms
}
