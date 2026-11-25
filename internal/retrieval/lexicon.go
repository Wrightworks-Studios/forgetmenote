package retrieval

import (
	"context"
	"errors"
	"math"
	"unsafe"
)

var ErrLexiconLimit = errors.New("lexical index exceeds its memory or frequency budget")

type frequency interface{ ~uint16 | ~uint32 }

// Posting counts one document once for IDF, independently of its field weights.
// Repository indexes retain compact uint16 counts; notebook counts use uint32.
type Posting[F frequency] struct {
	Document  uint32
	Frequency [4]F
}

// Lexicon owns immutable BM25 postings shared by notebook and repository search.
// Domain owners retain tokenization, field meaning, visibility and exact matches.
type Lexicon[F frequency] struct {
	postings map[string][]Posting[F]
	lengths  [][4]uint32
	averages [4]float64
	bytes    int64
}

// NewLexicon indexes at most four fields. The estimate includes document text,
// row overhead and posting capacity; it is an admission budget, not a heap size.
func NewLexicon[F frequency](ctx context.Context, count int, fields func(int) [4]string, tokenize func(string) []string, maxBytes int64) (*Lexicon[F], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count < 0 || uint64(count) > math.MaxUint32 || maxBytes < 1 || int64(count) > maxBytes/768 {
		return nil, ErrLexiconLimit
	}
	index := &Lexicon[F]{postings: map[string][]Posting[F]{}, lengths: make([][4]uint32, count)}
	postingBytes := int64(unsafe.Sizeof(Posting[F]{}))
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := fields(i)
		index.bytes += 768
		terms := map[string][4]F{}
		for field, value := range values {
			index.bytes += int64(len(value))
			if index.bytes > maxBytes {
				return nil, ErrLexiconLimit
			}
			tokens := tokenize(value)
			if uint64(len(tokens)) > math.MaxUint32 {
				return nil, ErrLexiconLimit
			}
			index.lengths[i][field] = uint32(len(tokens))
			index.averages[field] += float64(len(tokens))
			for _, term := range tokens {
				counts := terms[term]
				next := counts[field] + 1
				if next == 0 {
					return nil, ErrLexiconLimit
				}
				counts[field] = next
				terms[term] = counts
			}
		}
		for term, counts := range terms {
			prior := index.postings[term]
			if prior == nil {
				index.bytes += int64(len(term) + 96)
			}
			next := append(prior, Posting[F]{uint32(i), counts})
			index.bytes += int64(cap(next)-cap(prior)) * postingBytes
			if index.bytes > maxBytes {
				return nil, ErrLexiconLimit
			}
			index.postings[term] = next
		}
	}
	for field := range index.averages {
		if count > 0 {
			index.averages[field] /= float64(count)
		}
	}
	return index, nil
}

// EstimatedBytes reports the same admission estimate used while building.
func (index *Lexicon[F]) EstimatedBytes() int64 { return index.bytes }

func (index *Lexicon[F]) Postings(term string) []Posting[F] { return index.postings[term] }

// Scores applies visibility before scoring. Owners supply query-specific terms
// and protected exact-match semantics; duplicate terms never multiply scores.
func (index *Lexicon[F]) Scores(ctx context.Context, terms []string, weights [4]float64, accept func(int, string) bool) (map[int]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scores := map[int]float64{}
	seen := map[string]bool{}
	for _, term := range terms {
		if seen[term] {
			continue
		}
		seen[term] = true
		postings := index.postings[term]
		idf := IDF(len(index.lengths), len(postings))
		for _, posting := range postings {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			doc := int(posting.Document)
			if accept != nil && !accept(doc, term) {
				continue
			}
			for field, tf := range posting.Frequency {
				if tf != 0 {
					scores[doc] += TermScore(int(tf), int(index.lengths[doc][field]), index.averages[field], idf, weights[field])
				}
			}
		}
	}
	return scores, nil
}
