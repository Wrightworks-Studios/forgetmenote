package main

import (
	"bytes"
	"strings"
	"testing"

	"forgetmenote/internal/integrations"
)

func TestIntegrationsCommandReportsThisBuild(t *testing.T) {
	var out bytes.Buffer
	if err := runIntegrations(nil, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	all := integrations.All()
	if len(lines) != len(all) {
		t.Fatal(out.String())
	}
	for i, integration := range all {
		state := "included"
		if !integrations.Included(integration.Name) {
			state = "omitted"
		}
		if lines[i] != integration.Name+"\t"+state+"\t"+integration.Title {
			t.Fatal(lines[i])
		}
	}
	if err := runIntegrations([]string{"extra"}, &out); err == nil {
		t.Fatal("accepted an argument")
	}
}
