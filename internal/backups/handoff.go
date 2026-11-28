package backups

import (
	"context"
	"errors"
)

// BeginAuthority revokes outgoing leases and asks the configured storage owner's
// worker to acknowledge that its upload process has exited, for the GitHub
// binding and every connected drive destination of the notebook. Revocation
// alone is insufficient: an already accepted GitHub push or drive upload may
// still be finishing. The returned binding is Drained once every destination
// acknowledged.
func (m *Manager) BeginAuthority(ctx context.Context, project, id string, generation uint64, revision string) (Binding, error) {
	if err := m.lock(); err != nil {
		return Binding{}, err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	if b.Handoff == id && id != "" {
		b.Drained = b.Drained && m.drivesDrained(project, id)
		return b, nil
	}
	if !b.Connected || b.Generation != generation || b.LastRevision != revision || b.Handoff != "" || len(id) != 32 {
		return b, ErrConflict
	}
	for p, other := range m.state.Bindings {
		if p != project && (other.Connected || other.AuthorityTarget) && other.Repository.ID == b.Repository.ID && other.Branch == b.Branch {
			return b, errors.New("another notebook uses this GitHub branch")
		}
	}
	b.Handoff = id
	b.AuthorityTarget = true
	b.Enabled = false
	b.Drained = false
	b.Generation++
	leases, tokens := m.without(func(l Lease) bool { return l.Project == project })
	if err := m.commit(leases, []Binding{b}, m.fenceDrives(project, id)); err != nil {
		return b, err
	}
	m.revoke(ctx, tokens...)
	return b, nil
}
func (m *Manager) AcknowledgeDrain(owner, project, id string, generation uint64) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	if b.Owner != owner || b.Handoff != id || id == "" || b.Enabled || b.Generation != generation {
		return ErrDenied
	}
	b.Drained = true
	return m.save(m.leases, b)
}

// FinishAuthority ends handoff id. A committed handoff needs every
// destination's drain acknowledgement; it disconnects the drive destinations
// and revokes their grants, as the notebook now follows GitHub.
func (m *Manager) FinishAuthority(project, id string, commit bool) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	if b.Handoff == "" {
		if !commit || b.AuthorityTarget {
			return nil
		}
		return ErrConflict
	}
	if b.Handoff != id || commit && (!b.Drained || !m.drivesDrained(project, id)) {
		return ErrConflict
	}
	b.Handoff = ""
	b.Enabled = false
	b.AuthorityTarget = commit
	if commit {
		b.Connected = false
	}
	b.Generation++
	drives, grants := m.finishDrives(project, id, commit)
	if err := m.commit(m.leases, []Binding{b}, drives); err != nil {
		return err
	}
	for _, grant := range grants {
		_ = m.revokeDrive(context.Background(), grant.kind, grant.refresh)
	}
	return nil
}

// Returning to centrally editable notes never resumes the previous destination.
func (m *Manager) ReleaseAuthorityTarget(project string) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.mu.Unlock()
	b, ok := m.state.Bindings[project]
	if !ok || !b.AuthorityTarget {
		return nil
	}
	if b.Handoff != "" {
		return ErrConflict
	}
	b.AuthorityTarget = false
	b.Enabled = false
	b.Connected = false
	b.Generation++
	return m.save(m.leases, b)
}
