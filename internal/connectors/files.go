package connectors

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
)

// personalFile is where the file-based release kept every personal grant: one
// document sealed under the personal credential key and bound to no account.
// A gateway seals each grant as a row of its own instead.
const personalFile = "personal-connections/credentials.enc"

// personalFileState is the document credentials.enc seals.
type personalFileState struct {
	Version     int                           `json:"version"`
	Credentials map[string]personalCredential `json:"credentials"`
}

// PersonalFiles converts personal connection grants with the key Config names,
// as a gateway opens them. Config is the instance's integrations.personal, nil
// where personal connections are not configured.
type PersonalFiles struct{ Config *PersonalConfig }

// digest stands in for the grants' plaintext in the report: an HMAC under a
// key derived from the personal key, so the import and export reports still
// match while nobody without the key can test a guessed token against them.
func (f PersonalFiles) digest(canonical []byte) ([]byte, error) {
	key, err := personalKeyBytes(f.Config.KeyFile)
	if err != nil {
		return nil, err
	}
	derived, err := hkdf.Key(sha256.New, key, nil, "ForgetMeNote personal file report v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, derived)
	mac.Write(canonical)
	return mac.Sum(nil), nil
}

// store returns a store over db that only seals and opens grants.
func (f PersonalFiles) store(db *pg.DB) (*PersonalConnections, error) {
	if f.Config == nil {
		return nil, errors.New("personal connection grants need their key; configure integrations.personal as the file-based release had it")
	}
	if f.Config.Version != 1 || !filepath.IsAbs(f.Config.KeyFile) {
		return nil, errors.New("personal connections require version 1 and an absolute keyFile")
	}
	key, err := personalCipher(f.Config.KeyFile)
	if err != nil {
		return nil, err
	}
	return &PersonalConnections{db: db, key: key}, nil
}

// ImportFiles opens credentials.enc as the file-based release does and seals
// each grant as its row. A refresh that release left marked stays marked: it
// reads there as needing reauthorization, and a gateway records it so when it
// opens.
func (f PersonalFiles) ImportFiles(ctx context.Context, db *pg.DB, tx pgx.Tx, src *filestate.Source) (filestate.Sum, error) {
	var tally filestate.Tally
	raw, err := src.Read(personalFile)
	if errors.Is(err, fs.ErrNotExist) {
		return tally.Sum(), nil
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	p, err := f.store(db)
	if err != nil {
		return filestate.Sum{}, err
	}
	nonce := p.key.NonceSize()
	if len(raw) < nonce {
		return filestate.Sum{}, errors.New("cannot read personal credentials")
	}
	plain, err := p.key.Open(nil, raw[:nonce], raw[nonce:], []byte(personalSeal))
	var state personalFileState
	if err != nil || json.Unmarshal(plain, &state) != nil || state.Version != 1 || state.Credentials == nil {
		return filestate.Sum{}, errors.New("cannot decrypt personal credentials; restore the matching key")
	}
	rows := make([][]any, 0, len(state.Credentials))
	for _, id := range slices.Sorted(maps.Keys(state.Credentials)) {
		c := state.Credentials[id]
		if id != personalKey(c.Owner, c.Provider) || !validPersonalOwner(c.Owner) || c.Generation == 0 {
			return filestate.Sum{}, errors.New("invalid personal credential identity")
		}
		// The file-based release has no refresh holder and ignores one.
		c.RefreshHolder = ""
		sealed, err := p.sealCredential(c)
		if err != nil {
			return filestate.Sum{}, err
		}
		rows = append(rows, []any{db.Org, c.Owner, c.Provider, int64(1), sealed})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"personal_credentials"}, []string{"org", "owner", "provider", "version", "sealed"}, pgx.CopyFromRows(rows)); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(plain)
	if err == nil {
		canonical, err = f.digest(canonical)
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(personalFile, canonical)
	return tally.Sum(), nil
}

// ExportFiles seals every grant into one credentials.enc under the same key,
// with a fresh nonce as the file-based release writes it. Row versions only
// fence gateways' writes and are left behind.
func (f PersonalFiles) ExportFiles(ctx context.Context, db *pg.DB, q pg.Querier, out *filestate.Target) (filestate.Sum, error) {
	var tally filestate.Tally
	var grants int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM personal_credentials WHERE org = $1`, db.Org).Scan(&grants); err != nil || grants == 0 {
		return tally.Sum(), err
	}
	p, err := f.store(db)
	if err != nil {
		return filestate.Sum{}, err
	}
	stored, err := p.rows(ctx, q, "", "")
	if err != nil {
		return filestate.Sum{}, err
	}
	state := personalFileState{Version: 1, Credentials: make(map[string]personalCredential, len(stored))}
	for _, s := range stored {
		c := s.personalCredential
		// The refresh holder names a gateway, which the file-based release has
		// no field for. Every gateway has stopped, so a refresh still marked
		// was interrupted; that release reads it as needing reauthorization, as
		// a gateway does once the holder is gone.
		c.RefreshHolder = ""
		state.Credentials[personalKey(c.Owner, c.Provider)] = c
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return filestate.Sum{}, err
	}
	nonce := make([]byte, p.key.NonceSize())
	rand.Read(nonce)
	if err = out.Write(personalFile, p.key.Seal(nonce, nonce, plain, []byte(personalSeal))); err != nil {
		return filestate.Sum{}, err
	}
	canonical, err := filestate.JSON(plain)
	if err == nil {
		canonical, err = f.digest(canonical)
	}
	if err != nil {
		return filestate.Sum{}, err
	}
	tally.Add(personalFile, canonical)
	return tally.Sum(), nil
}
