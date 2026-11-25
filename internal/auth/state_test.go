package auth

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOpenStateRejectsFamilylessGrants(t *testing.T) {
	for _, kind := range []string{"access", "refresh", "usedRefresh"} {
		for _, missing := range []string{"family", "expiry"} {
			t.Run(kind+"/"+missing, func(t *testing.T) {
				grant := Grant{Family: "family", FamilyExpires: time.Now().Add(time.Hour).Unix()}
				if missing == "family" {
					grant.Family = ""
				} else {
					grant.FamilyExpires = 0
				}
				state := map[string]any{"clients": map[string]Client{}, "access": map[string]Grant{}, "refresh": map[string]Grant{}, kind: map[string]Grant{"hashed": grant}}
				raw, _ := json.Marshal(state)
				if _, err := storedState(t, string(raw)); err == nil || !strings.Contains(err.Error(), "invalid OAuth state") {
					t.Fatal("invalid family accepted", err)
				}
			})
		}
	}
}
func TestOpenStateBrowserSessionsDoNotRequireFamily(t *testing.T) {
	state := openTestState(t)
	state.Sessions = map[string]Grant{"session": {Subject: "browser", Expires: time.Now().Add(time.Hour).Unix()}}
	if err := state.Persist(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(state.db); err != nil {
		t.Fatal(err)
	}
}

type unavailableAuthority struct{}

func (unavailableAuthority) CheckSession(string, uint64) error { return catalog.ErrStorageUnavailable }
func TestRefreshDuringStorageFailureKeepsToken(t *testing.T) {
	state := openTestState(t)
	now := time.Now().Unix()
	grant := Grant{Subject: catalog.Bootstrap, Client: "test", Family: "family", FamilyExpires: now + 86400, Expires: now + 3600}
	state.Refresh[Digest("refresh")] = grant
	_, err := state.Exchange(unavailableAuthority{}, url.Values{"grant_type": {"refresh_token"}, "client_id": {"test"}, "refresh_token": {"refresh"}})
	if !errors.Is(err, ErrTemporarilyUnavailable) {
		t.Fatal("storage failure lost classification", err)
	}
	if state.Refresh[Digest("refresh")] != grant || len(state.Used) != 0 || len(state.Access) != 0 {
		t.Fatal("storage failure consumed refresh token")
	}
}

type revokedAuthority struct{}

func (revokedAuthority) CheckSession(string, uint64) error { return workspace.ErrForbidden }
func TestRefreshInvalidSessionDoesNotConsumeToken(t *testing.T) {
	state := openTestState(t)
	now := time.Now().Unix()
	grant := Grant{Subject: "alice", Client: "test", Family: "family", FamilyExpires: now + 86400, Expires: now + 3600}
	state.Refresh[Digest("refresh")] = grant
	_, err := state.Exchange(revokedAuthority{}, url.Values{"grant_type": {"refresh_token"}, "client_id": {"test"}, "refresh_token": {"refresh"}})
	if !errors.Is(err, ErrInvalidGrant) || state.Refresh[Digest("refresh")] != grant || len(state.Used) != 0 {
		t.Fatal("invalid session consumed refresh", err)
	}
}
