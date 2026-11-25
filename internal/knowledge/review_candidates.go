package knowledge

import (
	"context"
	"forgetmenote/internal/workspace"
)

// ReviewCandidates reuses the existing BM25 implementation for one authorized
// inventory. It is local to a job preparation, never shared across notebooks.
type ReviewCandidates struct {
	index  *bm25Index
	byPath map[string][]int
}

func NewReviewCandidates(ctx context.Context, passages []Passage) (*ReviewCandidates, error) {
	titles := map[string]string{}
	byPath := map[string][]int{}
	for i, passage := range passages {
		if titles[passage.Path+"\x00"+passage.NoteHash] == "" {
			titles[passage.Path+"\x00"+passage.NoteHash] = passage.Heading
		}
		byPath[passage.Path] = append(byPath[passage.Path], i)
	}
	index, err := newBM25(ctx, "review-inventory", passages, titles)
	if err != nil {
		return nil, err
	}
	return &ReviewCandidates{index: index, byPath: byPath}, nil
}

func (c *ReviewCandidates) Related(ctx context.Context, source Passage, limit int, complete bool) ([]Passage, error) {
	ranked, err := c.index.rank(ctx, source.Heading+" "+source.Text)
	if err != nil {
		return nil, err
	}
	ranks := map[int]passageRank{}
	for _, rank := range ranked {
		ranks[rank.index] = rank
	}
	for _, link := range noteLinks(workspace.SnapshotFile{Path: source.Path, Content: source.Text}, nil) {
		for _, index := range c.byPath[link.Path] {
			rank := ranks[index]
			rank.index = index
			rank.score += 100
			rank.exact = true
			ranks[index] = rank
		}
	}
	if !complete {
		indices := c.byPath[source.Path]
		for offset, index := range indices {
			if c.index.documents[index].passage.ID != source.ID {
				continue
			}
			for _, neighbor := range []int{offset - 1, offset + 1} {
				if neighbor >= 0 && neighbor < len(indices) {
					other := indices[neighbor]
					rank := ranks[other]
					rank.index = other
					rank.score += 50
					ranks[other] = rank
				}
			}
			break
		}
	}
	if complete {
		for index := range c.index.documents {
			if _, ok := ranks[index]; !ok {
				ranks[index] = passageRank{index: index}
			}
		}
	}
	ranked = ranked[:0]
	for _, rank := range ranks {
		if complete || c.index.documents[rank.index].passage.ID != source.ID {
			ranked = append(ranked, rank)
		}
	}
	c.index.sort(ranked)
	out := []Passage{}
	for _, rank := range ranked[:min(limit, len(ranked))] {
		out = append(out, c.index.documents[rank.index].passage)
	}
	return out, nil
}
