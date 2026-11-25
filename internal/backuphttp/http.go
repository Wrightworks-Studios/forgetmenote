// Package backuphttp owns the credential-restricted external backup worker API.
// Consent, generation fencing, drains, leases and durable receipts remain in the
// concrete backups manager; no HTTP argument selects a filesystem root.
package backuphttp

import (
	"crypto/sha256"
	"crypto/subtle"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/webcore"
	"maps"
	"net/http"
	"strings"
)

type Worker struct {
	Owner string
	hash  [32]byte
}

// NewWorker retains only a hash of the already validated operator credential.
func NewWorker(owner string, secret []byte) Worker {
	return Worker{Owner: owner, hash: sha256.Sum256(secret)}
}

type HTTP struct {
	manager *backups.Manager
	workers map[string]Worker
}

func New(manager *backups.Manager, workers map[string]Worker) *HTTP {
	return &HTTP{manager: manager, workers: maps.Clone(workers)}
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	credential, ok := h.workers[r.Header.Get("X-Backup-Worker")]
	supplied := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if !ok || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(supplied[:], credential.hash[:]) != 1 {
		http.Error(w, "Worker authentication required", 401)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	path := strings.TrimPrefix(r.URL.Path, "/internal/backups/v1/")
	m := h.manager
	if path == "bindings" && r.Method == "GET" {
		out, err := m.List(credential.Owner)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		webcore.JSON(w, out)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return
	}
	switch path {
	case "drain":
		var in struct {
			Project    string `json:"project"`
			Handoff    string `json:"handoff"`
			Generation uint64 `json:"generation"`
		}
		if !webcore.Decode(w, r, &in) {
			return
		}
		if err := m.AcknowledgeDrain(credential.Owner, in.Project, in.Handoff, in.Generation); err != nil {
			webcore.Fail(w, err)
			return
		}
		webcore.JSON(w, map[string]bool{"ok": true})
	case "lease":
		var in struct {
			Project    string `json:"project"`
			Generation uint64 `json:"generation"`
			Revision   string `json:"revision"`
		}
		if !webcore.Decode(w, r, &in) {
			return
		}
		out, err := m.Lease(r.Context(), credential.Owner, in.Project, in.Generation, in.Revision)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		webcore.JSON(w, out)
	case "check":
		var in struct {
			Lease string `json:"lease"`
		}
		if !webcore.Decode(w, r, &in) {
			return
		}
		if err := m.CheckLease(r.Context(), credential.Owner, in.Lease); err != nil {
			webcore.Fail(w, err)
			return
		}
		webcore.JSON(w, map[string]bool{"valid": true})
	case "receipt":
		var in backups.Receipt
		if !webcore.Decode(w, r, &in) {
			return
		}
		out, err := m.Complete(r.Context(), credential.Owner, in)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		webcore.JSON(w, out)
	default:
		http.NotFound(w, r)
	}
}
