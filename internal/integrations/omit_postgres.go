//go:build fmn_omit_postgres

package integrations

func init() { omitted["postgres"] = true }
