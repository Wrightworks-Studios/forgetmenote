package main

import (
	"strings"
	"testing"
)

func TestRemovedMCPGatewaySettingRequiresMigration(t *testing.T) {
	t.Setenv("MCP_ACCESS_CONFIG_FILE", "/not/read/legacy-gateway.json")
	err := runArguments([]string{"serve", "-data", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "MCP_ACCESS_CONFIG_FILE has been removed") || !strings.Contains(err.Error(), "/mcp") {
		t.Fatalf("expected actionable migration error, got %v", err)
	}
}
