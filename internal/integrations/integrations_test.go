package integrations

import (
	"strings"
	"testing"
)

func TestCheckNamesTheTagOfAnOmittedIntegration(t *testing.T) {
	for _, integration := range All() {
		err := Check(integration.Name)
		if Included(integration.Name) {
			if err != nil {
				t.Fatal(integration.Name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "fmn_omit_"+integration.Name) || !strings.Contains(err.Error(), integration.Title) {
			t.Fatal(integration.Name, err)
		}
	}
}
