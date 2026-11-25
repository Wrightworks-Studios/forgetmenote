package messaging

import "testing"

func TestWorkspaceMemberAudienceUsesOnlyDelegatedService(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shared   bool
		approved bool
		actors   []string
		valid    bool
	}{
		{"private", false, false, nil, true},
		{"shared", true, true, []string{"service-company"}, true},
		{"unapproved", true, false, []string{"service-company"}, false},
		{"missing-service", true, true, nil, false},
		{"other-actor", true, true, []string{"alice"}, false},
		{"extra-actor", true, true, []string{"service-company", "alice"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Client{Workspace: "team", Members: &MemberAccess{Actor: "service-company", Binding: Binding{Project: "company", ExternalProcessing: true, Shared: tc.shared, AudienceApproved: tc.approved, Actors: tc.actors}}}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			channel, _, member, ok := c.Resolve(Address{User: "employee", Channel: "any-channel"})
			if !ok || !member || channel.Shared != tc.shared {
				t.Fatal("workspace channel did not use configured audience")
			}
			dm, _, _, _ := c.Resolve(Address{User: "employee", Channel: "dm", Direct: true})
			if dm.Shared || dm.AudienceApproved || len(dm.Actors) != 0 {
				t.Fatal("workspace channel approval changed a DM audience")
			}
		})
	}
}

func TestPairByEmailNeedsWorkspaceDomains(t *testing.T) {
	client := Client{Workspace: "T1", Members: &MemberAccess{Actor: "service-1", Binding: Binding{Project: "default", ExternalProcessing: true}}}
	for _, domains := range [][]string{{"example.com"}, {"example.com", "corp.example.org"}} {
		client.Members.PairByEmail = domains
		if err := client.Validate(); err != nil {
			t.Fatalf("%q: %v", domains, err)
		}
	}
	for _, domains := range [][]string{{"Example.com"}, {"example"}, {"@example.com"}, {""}, {"example.com/x"}} {
		client.Members.PairByEmail = domains
		if client.Validate() == nil {
			t.Fatalf("%q was accepted as a pairing domain", domains)
		}
	}
}
