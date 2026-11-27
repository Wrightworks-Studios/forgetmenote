package modules

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"forgetmenote/internal/integrations"
)

// Without a modules section, or with an empty off list, every module is on.
func TestTheDefaultSetTurnsEveryModuleOn(t *testing.T) {
	parsed, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Parse([]string{})
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []Set{{}, parsed, empty} {
		for _, module := range All() {
			if !set.Enabled(module.Name) || set.Check(module.Name) != nil {
				t.Fatalf("%s is off in the default set", module.Name)
			}
		}
		states := set.States()
		if len(states) != len(All()) {
			t.Fatal(states)
		}
		for i, state := range states {
			if state.Name != All()[i].Name || state.Title != All()[i].Title || !state.Enabled {
				t.Fatal(state)
			}
		}
	}
}

// A misspelt or retired name is refused rather than ignored, so an operator
// never believes a module is off while it runs.
func TestAnUnknownModuleNameIsRefused(t *testing.T) {
	for _, name := range []string{"jev", "Chat", "", "tasks "} {
		_, err := Parse([]string{name})
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(name)) || !strings.Contains(err.Error(), "not a module") {
			t.Fatalf("%q: %v", name, err)
		}
	}
}

// Each module a module cannot run without may be turned off only with it.
// The refusal names both, whatever else the list turns off.
func TestTheDependencyCheckRefusesEachBrokenRequirementAndNamesBothModules(t *testing.T) {
	for _, requirement := range []struct{ module, requires string }{
		{"tasks", "automations"},
		{"messaging", "automations"},
		{"messaging", "tasks"},
		{"messaging", "memory"},
		{"messaging", "chat"},
		{"personal", "automations"},
		{"personal", "messaging"},
	} {
		// Turning off the other modules that require it too leaves this
		// module as the only one the check can name.
		off := []string{requirement.requires}
		for _, module := range All() {
			if module.Name != requirement.module && slices.Contains(module.Requires, requirement.requires) {
				off = append(off, module.Name)
			}
		}
		_, err := Parse(off)
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(requirement.module)) || !strings.Contains(err.Error(), strconv.Quote(requirement.requires)) || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("%s without %s (off %v): %v", requirement.module, requirement.requires, off, err)
		}
	}
}

// Every module can be turned off with the modules that require it, so the
// modules nothing requires each alone, and every module can be turned off at
// once.
func TestEveryModuleCanBeTurnedOff(t *testing.T) {
	for _, module := range All() {
		off := map[string]bool{module.Name: true}
		for changed := true; changed; {
			changed = false
			for _, other := range All() {
				if !off[other.Name] && slices.ContainsFunc(other.Requires, func(required string) bool { return off[required] }) {
					off[other.Name], changed = true, true
				}
			}
		}
		var list []string
		for name := range off {
			list = append(list, name)
		}
		set, err := Parse(list)
		if err != nil {
			t.Fatalf("%s with %v: %v", module.Name, list, err)
		}
		for _, other := range All() {
			if set.Enabled(other.Name) == off[other.Name] {
				t.Fatalf("with %v off, %s is %v", list, other.Name, set.Enabled(other.Name))
			}
		}
	}
	every, err := Parse(names(All()))
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range All() {
		if every.Enabled(module.Name) {
			t.Fatalf("with every module off, %s is on", module.Name)
		}
	}
	one, _ := Parse([]string{"sources"})
	if !every.Equal(every) || every.Equal(one) || one.Equal(Set{}) || !(Set{}).Equal(Set{off: map[string]bool{}}) {
		t.Fatal("Equal compares the modules a set runs")
	}
}

// An instance that turns a module off says so wherever the module is asked
// for, naming the module and the setting that turned it off.
func TestASetReportsTheModulesItTurnsOff(t *testing.T) {
	set := Set{off: map[string]bool{"personal": true}}
	if set.Enabled("personal") || !set.Enabled("chat") || set.Check("chat") != nil {
		t.Fatal("wrong modules on")
	}
	err := set.Check("personal")
	if err == nil || !strings.Contains(err.Error(), "Personal assistants") || !strings.Contains(err.Error(), `"personal"`) || !strings.Contains(err.Error(), "modules.off") || !strings.Contains(err.Error(), "instance.json") {
		t.Fatal(err)
	}
	for _, state := range set.States() {
		if state.Enabled == (state.Name == "personal") {
			t.Fatal(state)
		}
	}
	if got := set.String(); !strings.Contains(got, "off: personal") || strings.Contains(got, "on: personal") {
		t.Fatal(got)
	}
	if got := (Set{}).String(); !strings.HasPrefix(got, "on: chat, automations,") || !strings.HasSuffix(got, "; off: none") {
		t.Fatal(got)
	}
}

// The registry's requirements and its integrations name things that exist,
// and every optional integration belongs to exactly one module, so turning a
// module off can report its integrations as off.
func TestTheRegistryNamesOnlyModulesAndIntegrationsThatExist(t *testing.T) {
	names := map[string]bool{}
	for _, module := range All() {
		if names[module.Name] || module.Title == "" {
			t.Fatalf("%q is listed twice or has no title", module.Name)
		}
		names[module.Name] = true
	}
	owner := map[string]string{}
	for _, module := range All() {
		for _, required := range module.Requires {
			if !names[required] || required == module.Name {
				t.Fatalf("%s requires %q", module.Name, required)
			}
		}
		for _, integration := range module.Integrations {
			if previous, ok := owner[integration]; ok {
				t.Fatalf("%s belongs to %s and %s", integration, previous, module.Name)
			}
			owner[integration] = module.Name
		}
	}
	for _, integration := range integrations.All() {
		if owner[integration.Name] == "" {
			t.Fatalf("integration %s belongs to no module", integration.Name)
		}
		delete(owner, integration.Name)
	}
	if len(owner) != 0 {
		t.Fatalf("modules name integrations that do not exist: %v", owner)
	}
}
