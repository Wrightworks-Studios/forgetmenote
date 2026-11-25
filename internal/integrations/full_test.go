//go:build !fmn_omit_postgres && !fmn_omit_redshift && !fmn_omit_outlook && !fmn_omit_iwork && !fmn_omit_ocr

package integrations

import "testing"

// The default build, which the release image uses, includes every integration.
func TestDefaultBuildIncludesEveryIntegration(t *testing.T) {
	if omitted := Omitted(); len(omitted) != 0 {
		t.Fatal(omitted)
	}
}
