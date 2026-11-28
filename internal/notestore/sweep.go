package notestore

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
)

// minSweepAge keeps the sweep away from blocks a write may still be about to
// reference: no write holds its transaction this long.
var minSweepAge = 10 * time.Minute

// The scheduled block sweep. Every open store tries to sweep about
// blockSweepDelay after it opens and then every blockSweepEvery, and one
// gateway of the org at a time does (blockSweepLock), so the blocks a purge
// failed to remove, or a write refused after it put them, leave the blob
// store within about a day. A delete asks its store to sweep
// blockSweepSoon after it, once the blocks it freed, which it touched, are
// past the grace period, unless a sweep is due sooner. A store that finds
// the lock held, by another gateway's sweep, a purge's block removal or an
// instance capture, tries again blockSweepRetry later rather than a day
// later, so a capture that runs at the hour of a gateway's sweep every day
// does not keep that sweep from ever running.
var (
	blockSweepDelay   = 5 * time.Minute
	blockSweepJitter  = time.Minute
	blockSweepEvery   = 24 * time.Hour
	blockSweepSoon    = minSweepAge + time.Minute
	blockSweepRetry   = 15 * time.Minute
	blockSweepTimeout = time.Hour
)

// blockSweepLock guards removing content blocks from the blob store. The
// sweep takes it exclusively without waiting, a purge's removal of the
// blocks it freed shares it, waiting, and an instance capture (LockBlocks)
// holds it exclusively while it copies the blocks of its database snapshot,
// so nothing removes a block the snapshot uses until the capture ends.
const blockSweepLock = "notestore-block-sweep"

// sweepBatch is how many blocks one transaction of a sweep locks while it
// deletes their objects one by one, which a write that uses one of them
// again waits for.
const sweepBatch = 100

// sweeper runs the scheduled block sweep until ctx ends. The first delay has
// up to blockSweepJitter more, so gateways started together do not all try
// at once.
func (s *Store) sweeper(ctx context.Context) {
	delay := blockSweepDelay
	if blockSweepJitter > 0 {
		delay += rand.N(blockSweepJitter)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	next := time.Now().Add(delay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.sweepSoon:
			if soon := time.Now().Add(blockSweepSoon); soon.Before(next) {
				timer.Reset(blockSweepSoon)
				next = soon
			}
			continue
		case <-timer.C:
		}
		wait := blockSweepEvery
		if _, swept, err := s.scheduledSweep(ctx); err == nil && !swept {
			wait = min(blockSweepRetry, blockSweepEvery)
		}
		timer.Reset(wait)
		next = time.Now().Add(wait)
	}
}

// sweepAfterDelete asks the sweeper for a sweep blockSweepSoon from now.
func (s *Store) sweepAfterDelete() {
	select {
	case s.sweepSoon <- struct{}{}:
	default: // already asked
	}
}

// scheduledSweep runs SweepBlocks unless another gateway of the org is
// sweeping, and logs what it removed or why it failed. swept is false when
// another gateway held the sweep.
func (s *Store) scheduledSweep(ctx context.Context) (SweepResult, bool, error) {
	run, cancel := context.WithTimeout(ctx, blockSweepTimeout)
	defer cancel()
	var result SweepResult
	release, swept, err := s.db.TryExclusive(run, blockSweepLock)
	if err == nil && swept {
		result, err = s.SweepBlocks(run, minSweepAge)
		release()
	}
	switch {
	case err != nil && ctx.Err() == nil: // not a store that is closing
		log.Printf("notes store block sweep failed; unused content blocks stay in the blob store until the next sweep: %v", err)
	case result.Segments > 0 || result.Blocks > 0:
		log.Printf("notes store block sweep removed %d unused content segments and %d blocks", result.Segments, result.Blocks)
	}
	return result, swept, err
}

// purgeAfterCommit removes, at once rather than at the next sweep, the
// segments a committed purge freed and the blocks only they used
// (purgeSegments). It runs without the caller's cancellation, so a client
// that disconnects once the commit is done cannot stop it, and tries three
// times; whatever it could not remove stays in the blob store until the
// scheduled block sweep removes it. It shares the sweep's lock, waiting for
// a running sweep or instance capture within the same budget: a capture's
// database snapshot may still use the blocks the purge freed.
func (s *Store) purgeAfterCommit(ctx context.Context, segments [][32]byte) (removedSegments, removedBlocks int, err error) {
	if len(segments) == 0 {
		return 0, 0, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeTimeout)
	defer cancel()
	release, err := s.db.Shared(ctx, blockSweepLock)
	if err != nil {
		return 0, 0, fmt.Errorf("waiting for a block sweep or an instance capture to end: %w", err)
	}
	defer release()
	for attempt := 1; ; attempt++ {
		if removedSegments, removedBlocks, err = s.purgeSegments(ctx, segments); err == nil || attempt == 3 {
			return removedSegments, removedBlocks, err
		}
		select {
		case <-ctx.Done():
			return 0, 0, err
		case <-time.After(time.Duration(attempt) * purgeRetry):
		}
	}
}

// purgeTimeout bounds purgeAfterCommit's wait for the lock and its attempts
// together; purgeRetry is the pause before the second, twice it before the
// third.
var (
	purgeTimeout = 5 * time.Minute
	purgeRetry   = time.Second
)

// A SweepResult counts what a sweep removed.
type SweepResult struct {
	Segments, Blocks int
}

// SweepBlocks removes content segments no file version uses and blocks no
// segment uses, once no write has touched them for olderThan (at least ten
// minutes). A write records each block it puts and touches each segment it
// stores, and a deleted file version touches its own, so a write in progress
// never loses one.
func (s *Store) SweepBlocks(ctx context.Context, olderThan time.Duration) (SweepResult, error) {
	var result SweepResult
	org := s.db.Org
	age := max(olderThan, minSweepAge)
	rows, err := s.db.Pool.Query(ctx, `SELECT segments FROM file_versions WHERE org = $1 AND segments IS NOT NULL`, org)
	if err != nil {
		return result, err
	}
	used := map[[32]byte]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return result, err
		}
		segments, err := decodeSegments(raw)
		if err != nil {
			rows.Close()
			return result, err
		}
		for _, seg := range segments {
			used[seg.sha] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	// Blocks of a used or recent segment stay; a recent segment may belong to
	// a write that has not committed its file version yet.
	rows, err = s.db.Pool.Query(ctx, `SELECT segment_sha, blocks, touched < now() - $2::interval FROM content_segments WHERE org = $1`, org, age)
	if err != nil {
		return result, err
	}
	keep := map[[32]byte]bool{}
	doomed := map[[32]byte][]byte{}
	for rows.Next() {
		var sha, body []byte
		var old bool
		if err := rows.Scan(&sha, &body, &old); err != nil {
			rows.Close()
			return result, err
		}
		if old && !used[[32]byte(sha)] {
			doomed[[32]byte(sha)] = body
			continue
		}
		if err := keepBlocks(keep, body); err != nil {
			rows.Close()
			return result, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(doomed) > 0 {
		keys := make([][]byte, 0, len(doomed))
		for sha := range doomed {
			keys = append(keys, append([]byte(nil), sha[:]...))
		}
		rows, err := s.db.Pool.Query(ctx, `DELETE FROM content_segments WHERE org = $1 AND segment_sha = ANY($2) AND touched < now() - $3::interval RETURNING segment_sha`, org, keys, age)
		if err != nil {
			return result, err
		}
		deleted, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return result, err
		}
		result.Segments = len(deleted)
		for _, sha := range deleted {
			delete(doomed, [32]byte(sha))
		}
		for _, body := range doomed { // touched again since it was read
			if err := keepBlocks(keep, body); err != nil {
				return result, err
			}
		}
	}
	rows, err = s.db.Pool.Query(ctx, `SELECT block_sha FROM content_blocks WHERE org = $1 AND touched < now() - $2::interval`, org, age)
	if err != nil {
		return result, err
	}
	old, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return result, err
	}
	var candidates [][]byte
	for _, sha := range old {
		if !keep[[32]byte(sha)] {
			candidates = append(candidates, sha)
		}
	}
	for start := 0; start < len(candidates); start += sweepBatch {
		batch := candidates[start:min(start+sweepBatch, len(candidates))]
		// The rows stay locked while their objects go, so a write that
		// records one of these blocks again waits and then puts it again.
		err := pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT block_sha FROM content_blocks WHERE org = $1 AND block_sha = ANY($2) AND touched < now() - $3::interval FOR UPDATE SKIP LOCKED`, org, batch, age)
			if err != nil {
				return err
			}
			locked, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
			if err != nil || len(locked) == 0 {
				return err
			}
			for _, sha := range locked {
				if err := s.blobs.Delete(ctx, blockKey([32]byte(sha))); err != nil {
					return err
				}
				s.blocks.remove([32]byte(sha))
			}
			if _, err := tx.Exec(ctx, `DELETE FROM content_blocks WHERE org = $1 AND block_sha = ANY($2)`, org, locked); err != nil {
				return err
			}
			result.Blocks += len(locked)
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func keepBlocks(keep map[[32]byte]bool, body []byte) error {
	refs, err := decodeBlocks(body)
	for _, r := range refs {
		keep[r.sha] = true
	}
	return err
}
