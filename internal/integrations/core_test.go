//go:build fmn_omit_postgres && fmn_omit_redshift && fmn_omit_outlook && fmn_omit_slack

package integrations

import "testing"

// Each tag leaves out the integration it names, so all of them leave out all.
func TestCoreBuildOmitsEveryIntegration(t *testing.T) {
	for _, integration := range All() {
		if Included(integration.Name) {
			t.Fatal(integration.Name)
		}
	}
}
