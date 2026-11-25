package workspace

import (
	"encoding/json"
	"errors"
	"sort"
)

const locksPath = ".gitnotes-locks.json"

// maxLockPolicyBytes bounds the lock policy a notebook reads.
const maxLockPolicyBytes = 65536

type LockRule struct {
	Path string `json:"path"`
}
type LockState struct {
	Path     string     `json:"path"`
	Revision string     `json:"revision"`
	Rules    []LockRule `json:"rules"`
}
type lockPolicy struct {
	Version int      `json:"version"`
	Paths   []string `json:"paths"`
}

// parseLockPolicy parses the lock policy as loadLocks reads it: version 1,
// with at most 256 paths a notebook may hold, none twice.
func (s *Store) parseLockPolicy(raw []byte) ([]string, error) {
	var policy lockPolicy
	if err := json.Unmarshal(raw, &policy); err != nil || policy.Version != 1 || policy.Paths == nil || len(policy.Paths) > 256 {
		return nil, errors.New("invalid project lock policy")
	}
	seen := map[string]bool{}
	for _, path := range policy.Paths {
		if _, err := s.resolve(path, false); err != nil {
			return nil, err
		}
		if seen[path] {
			return nil, errors.New("duplicate project lock path")
		}
		seen[path] = true
	}
	return policy.Paths, nil
}

// Covering returns the lock rule that protects path, or "" when none does.
// It uses the same prefix matching as the editability check.
func (l LockState) Covering(path string) string {
	for _, rule := range l.Rules {
		if matchesPrefix(path, []string{rule.Path}) {
			return rule.Path
		}
	}
	return ""
}

func (s *Store) lockRules(path, revision string) LockState {
	state := LockState{Path: path, Revision: revision, Rules: []LockRule{}}
	for _, prefix := range s.projectLocks {
		if path == "" || matchesPrefix(path, []string{prefix}) {
			state.Rules = append(state.Rules, LockRule{prefix})
		}
	}
	sort.Slice(state.Rules, func(i, j int) bool { return state.Rules[i].Path < state.Rules[j].Path })
	return state
}
