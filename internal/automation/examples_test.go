package automation

import "testing"

func TestGenericAutomationExamples(t *testing.T) {
	c, err := Load("../../deploy/integrations/automations.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range c.Jobs {
		if err := spec.ValidateConfigured(); err != nil {
			t.Fatalf("%s: %v", spec.ID, err)
		}
	}
}
