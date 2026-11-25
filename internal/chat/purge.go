package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// RedactProject gives fn the stored record of every conversation that may
// quote project's content, whoever owns it, as a notebook purge cleans the
// private transcripts that quoted the purged content without anyone reading
// them: the conversations of project, and those of other notebooks whose
// corpus, or a run's corpus, drew on it. fn returns the record's new bytes
// and whether it changed. With write, each changed record replaces the stored
// one when that is still the version fn read, retrying on a newer one, and a
// recovery overlay is bound to the new bytes, so a stream on any gateway
// reads the redacted transcript; otherwise nothing changes. It returns how
// many conversations fn changed. A record fn refuses, or whose redaction no
// longer decodes as the same conversation, is left as it was, so no
// conversation becomes unreadable; the others are still redacted and the
// error counts them.
func (s *Store) RedactProject(ctx context.Context, project string, fn func([]byte) ([]byte, bool, error), write bool) (int, error) {
	if project == "" || fn == nil {
		return 0, errors.New("a redaction needs the notebook and a function")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// A record naming the notebook, as its project or in its corpus, holds
	// its ID as a JSON string.
	name, _ := json.Marshal(project)
	rows, err := s.db.Pool.Query(ctx, `SELECT account, id FROM chat_conversations WHERE org = $1 AND position($2::bytea IN record) > 0 ORDER BY account, id`, s.db.Org, name)
	if err != nil {
		return 0, err
	}
	type key struct{ account, id string }
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (key, error) {
		var k key
		return k, row.Scan(&k.account, &k.id)
	})
	if err != nil {
		return 0, err
	}
	changed, skipped := 0, 0
	var refused error
	for _, k := range keys {
		for attempt := 0; ; attempt++ {
			var version int64
			var record, recovery []byte
			err := s.db.Pool.QueryRow(ctx, `SELECT version, record, recovery FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`,
				s.db.Org, k.account, k.id).Scan(&version, &record, &recovery)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				return changed, err
			}
			var head struct {
				ID      string        `json:"id"`
				Project string        `json:"project"`
				Options corpusOptions `json:"options"`
				Runs    []struct {
					Options corpusOptions `json:"options"`
				} `json:"runs"`
			}
			if json.Unmarshal(record, &head) != nil {
				break
			}
			drew := head.Options.draws(project)
			for _, run := range head.Runs {
				drew = drew || run.Options.draws(project)
			}
			if head.Project != project && !drew {
				break
			}
			next, redacted, err := fn(record)
			if err != nil {
				skipped, refused = skipped+1, err
				break
			}
			if !redacted || bytes.Equal(next, record) {
				break
			}
			if !write {
				changed++
				break
			}
			var c Conversation
			d := json.NewDecoder(bytes.NewReader(next))
			d.DisallowUnknownFields()
			if d.Decode(&c) != nil || c.ID != head.ID || c.Project != head.Project {
				skipped++
				break
			}
			if recovery != nil {
				var overlay recoveryRecord
				if json.Unmarshal(recovery, &overlay) == nil && overlay.Hash == Hash(record) {
					overlay.Hash = Hash(next)
					if recovery, err = json.Marshal(overlay); err != nil {
						return changed, err
					}
				} else {
					// An overlay bound to other bytes was already stale.
					recovery = nil
				}
			}
			tag, err := s.db.Pool.Exec(ctx, `UPDATE chat_conversations SET version = version + 1, record = $5, recovery = $6
				WHERE org = $1 AND account = $2 AND id = $3 AND version = $4`, s.db.Org, k.account, k.id, version, next, recovery)
			if err != nil {
				return changed, err
			}
			if tag.RowsAffected() == 1 {
				changed++
				break
			}
			if attempt >= 20 {
				return changed, ErrConflict
			}
		}
	}
	if skipped > 0 {
		return changed, errors.Join(fmt.Errorf("%d chat transcripts cannot be redacted and are left as they are", skipped), refused)
	}
	return changed, nil
}

// corpusOptions is the corpus of a conversation's or a run's options.
type corpusOptions struct {
	Corpus *struct {
		NoteProjects   []string `json:"noteProjects"`
		SourceProjects []string `json:"sourceProjects"`
	} `json:"corpus"`
}

// draws reports whether the corpus draws on project.
func (o corpusOptions) draws(project string) bool {
	return o.Corpus != nil && (slices.Contains(o.Corpus.NoteProjects, project) || slices.Contains(o.Corpus.SourceProjects, project))
}
