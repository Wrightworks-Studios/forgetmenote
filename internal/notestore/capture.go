package notestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/pg"
)

// An instance capture (internal/capture) copies the content blocks that the
// file versions of one database snapshot use while the instance keeps
// serving. A block is put into the blob store before the transaction that
// first references it commits and never changes; only the block sweep and a
// purge's removal of the blocks it freed delete one, and both take
// blockSweepLock. So a capture that holds the lock before its snapshot
// begins finds every block the snapshot uses until it releases it.

// LockBlocks takes blockSweepLock for org on conn as a session lock, waiting
// until ctx ends for a running sweep or purge block removal, and keeps both
// out until unlock runs. A session lock, rather than the transaction lock the
// sweep takes, lets the caller begin its snapshot on the same connection
// only once it holds the lock, so no removal that read the file versions
// before the snapshot runs during it, and a connection that drops ends the
// lock and the snapshot together. If unlock fails the lock lasts as long as
// the connection: close it rather than return it to a pool.
func LockBlocks(ctx context.Context, conn *pgx.Conn, org string) (unlock func(context.Context) error, err error) {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1 || ' ' || $2, 0))`, blockSweepLock, org); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		var released bool
		if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1 || ' ' || $2, 0))`, blockSweepLock, org).Scan(&released); err != nil {
			return err
		}
		if !released {
			return fmt.Errorf("the %s lock was not held", blockSweepLock)
		}
		return nil
	}, nil
}

// A Block is a content block in the blob store: the SHA-256 and size of its
// bytes, and its key.
type Block struct {
	Key    string
	SHA256 [32]byte
	Size   int64
}

// ReferencedBlocks returns every content block that the file versions of
// org visible to q use, once each, in key order. Blocks no file version
// uses, such as those of a write that has not committed or that a purge
// freed, are not listed. A file version whose segment q cannot find, or
// whose segment's body does not match its hash, is an error.
func ReferencedBlocks(ctx context.Context, q pg.Querier, org string) ([]Block, error) {
	rows, err := q.Query(ctx, `SELECT segments FROM file_versions WHERE org = $1 AND segments IS NOT NULL`, org)
	if err != nil {
		return nil, err
	}
	used := map[[32]byte]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		segments, err := decodeSegments(raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		for _, seg := range segments {
			used[seg.sha] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT segment_sha, blocks FROM content_segments WHERE org = $1`, org)
	if err != nil {
		return nil, err
	}
	sizes := map[[32]byte]uint32{}
	found := 0
	for rows.Next() {
		var sha, body []byte
		if err := rows.Scan(&sha, &body); err != nil {
			rows.Close()
			return nil, err
		}
		if len(sha) != 32 || !used[[32]byte(sha)] {
			continue
		}
		if sha256.Sum256(body) != [32]byte(sha) {
			rows.Close()
			return nil, fmt.Errorf("content segment %x is corrupt", sha)
		}
		refs, err := decodeBlocks(body)
		if err != nil {
			rows.Close()
			return nil, err
		}
		for _, r := range refs {
			sizes[r.sha] = r.size
		}
		found++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != len(used) {
		return nil, fmt.Errorf("%d content segments that file versions use are missing", len(used)-found)
	}
	blocks := make([]Block, 0, len(sizes))
	for sha, size := range sizes {
		blocks = append(blocks, Block{Key: blockKey(sha), SHA256: sha, Size: int64(size)})
	}
	slices.SortFunc(blocks, func(a, b Block) int { return bytes.Compare(a.SHA256[:], b.SHA256[:]) })
	return blocks, nil
}
