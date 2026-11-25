package connectors

import (
	"strings"
	"testing"

	"forgetmenote/internal/integrations"
)

// A build that leaves out a database adapter refuses its connections by name
// at startup; the full build accepts the same configuration.
func TestDatabaseConnectionsNeedTheirIntegration(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	operations := []Operation{{Name: "query", ReadOnly: true}, {Name: "schema", ReadOnly: true}}
	for _, c := range []Connection{
		{ID: "warehouse", Kind: "postgres", Endpoint: "postgres://reader@127.0.0.1:5432/analytics?sslmode=disable", SecretEnv: "FMN_TEST_UNUSED", Operations: operations},
		{ID: "lake", Kind: "redshift", Redshift: &RedshiftConfig{Region: "us-east-1", Database: "analytics", Workgroup: "readonly"}, Operations: operations},
	} {
		m, err := New(Config{Version: 1, Connections: []Connection{c}})
		if integrations.Included(c.Kind) {
			if err != nil {
				t.Fatal(c.Kind, err)
			}
			m.Close()
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "connection "+c.ID) || !strings.Contains(err.Error(), "fmn_omit_"+c.Kind) {
			t.Fatal(c.Kind, err)
		}
	}
}
