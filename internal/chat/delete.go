package chat

import (
	"errors"
	"strings"
)

var ErrDeleteActive = errors.New("stop the conversation and wait for it to finish stopping before deleting it")

// Catalog notebook IDs cannot contain a colon. Keeping accounting in the old
// Conversation schema under this unreachable notebook preserves accounting
// across application upgrades and compatible image rollbacks.
const deletedProjectPrefix = "deleted:"

func deletedProject(project string) bool { return strings.HasPrefix(project, deletedProjectPrefix) }

// authorize rechecks the caller at the mutation boundary after any store-lock
// wait. Storage-only callers may pass nil when authority is already established.
// retired reports only a matching durable receipt, even if final sync or cleanup
// failed; callers must fence cached content as soon as that boundary is crossed.
func (s *Store) Delete(user, project, id string, authorize func() error) (retired bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if authorize != nil {
		if err = authorize(); err != nil {
			return false, err
		}
	}
	if deletedProject(project) {
		return false, ErrNotFound
	}
	c, version, err := s.loadVersion(user, project, id)
	if errors.Is(err, ErrNotFound) {
		c, err = s.load(user, deletedProjectPrefix+project, id)
		if err != nil {
			return false, err
		}
		if authorize != nil {
			if err = authorize(); err != nil {
				return true, err
			}
		}
		return true, s.cleanDeleted(c)
	}
	if err != nil {
		return false, err
	}
	for _, r := range c.Runs {
		if Active(r.State) || r.InFlightTokens > 0 {
			return false, ErrDeleteActive
		}
	}
	receipt := Conversation{Version: 1, ID: c.ID, Owner: c.Owner, Project: deletedProjectPrefix + c.Project, CreatedAt: c.CreatedAt, UpdatedAt: Now(), Turns: []Turn{}, Runs: []Run{}, Events: []EventRecord{}}
	for _, r := range c.Runs {
		ensureUsageDays(&r)
		r.Usage.Reserved = 0
		receipt.Runs = append(receipt.Runs, Run{ID: r.ID, State: "completed", CreatedAt: r.CreatedAt, Options: Options{Connection: r.Options.Connection}, Usage: r.Usage, UsageDays: r.UsageDays})
	}
	if authorize != nil {
		if err = authorize(); err != nil {
			return false, err
		}
	}
	if err = s.persistReserved(receipt, version); err != nil {
		// A write can commit although its reply was lost. Match the receipt
		// itself: a different notebook's record cannot retire a stream.
		_, receiptErr := s.load(user, deletedProjectPrefix+project, id)
		return receiptErr == nil, err
	}
	return true, s.cleanDeleted(receipt)
}

// cleanDeleted drops whatever private state a receipt superseded: today only a
// recovery overlay naming the deleted conversation's runs, which the receipt's
// own write also clears.
func (s *Store) cleanDeleted(c Conversation) error {
	ctx, cancel := storageContext()
	defer cancel()
	_, err := s.db.Pool.Exec(ctx, `UPDATE chat_conversations SET recovery = NULL WHERE org = $1 AND account = $2 AND id = $3`, s.db.Org, Hash([]byte(c.Owner)), c.ID)
	return err
}
