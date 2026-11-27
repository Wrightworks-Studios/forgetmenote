// Package modules lists the feature modules an instance can turn off and
// decides at startup which are on.
//
// Every module is on unless instance.json's modules.off names it. The list is
// fixed in code, like internal/integrations: nothing registers at runtime, and
// the release image runs every module. A module that is off is absent on every
// transport while its stored data stays. Each module's wiring lands
// separately, and until it has, modules.off refuses to name it.
package modules

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Module struct {
	// Name is the module's word in modules.off.
	Name  string `json:"name"`
	Title string `json:"title"`
	// Requires names the modules this one cannot run without. Every other
	// use of a module is soft and answers "not available" while it is off.
	Requires []string `json:"requires,omitempty"`
	// Integrations names the internal/integrations entries that belong to
	// this module, so that they report as off with it.
	Integrations []string `json:"integrations,omitempty"`
	// wired is set once nothing of the module runs while it is off: its
	// hubs, routes, tools and workers skip themselves. Until then modules.off
	// refuses the module rather than leave it running.
	wired bool
}

var all = []Module{
	{Name: "chat", Title: "Notebook chat"},
	{Name: "automations", Title: "Automations"},
	{Name: "judgments", Title: "Judgments", wired: true},
	{Name: "data-connections", Title: "Data connections", Integrations: []string{"postgres", "redshift"}, wired: true},
	{Name: "sources", Title: "Archived sources", Integrations: []string{"outlook"}, wired: true},
	{Name: "repositories", Title: "Repository notebooks"},
	{Name: "tasks", Title: "Task queues", Requires: []string{"automations"}},
	{Name: "messaging", Title: "Slack messaging", Requires: []string{"automations", "tasks", "memory", "chat"}, Integrations: []string{"slack"}},
	{Name: "personal", Title: "Personal assistants", Requires: []string{"automations", "messaging"}},
	{Name: "memory", Title: "Agent memory"},
	{Name: "managed-backups", Title: "Managed backups"},
}

// All returns every module in the registry's order.
func All() []Module { return append([]Module(nil), all...) }

func find(name string) (Module, bool) {
	for _, module := range all {
		if module.Name == name {
			return module, true
		}
	}
	return Module{}, false
}

// Set is which modules an instance runs. The zero Set runs every module.
type Set struct{ off map[string]bool }

// Parse checks a modules.off list. It refuses an unknown name, then a module
// left on while one it requires is off, naming both, and only then a module
// whose wiring has not landed yet.
func Parse(off []string) (Set, error) {
	set := Set{off: map[string]bool{}}
	for _, name := range off {
		if _, ok := find(name); !ok {
			return Set{}, fmt.Errorf("modules.off names %q, which is not a module; the modules are %s", name, strings.Join(names(all), ", "))
		}
		set.off[name] = true
	}
	var broken []error
	for _, module := range all {
		if set.off[module.Name] {
			continue
		}
		for _, required := range module.Requires {
			if set.off[required] {
				broken = append(broken, fmt.Errorf("module %q requires %q, which modules.off turns off; turn %q off too, or %q back on", module.Name, required, module.Name, required))
			}
		}
	}
	if len(broken) > 0 {
		return Set{}, errors.Join(broken...)
	}
	var unwired []Module
	for _, module := range all {
		if set.off[module.Name] && !module.wired {
			unwired = append(unwired, module)
		}
	}
	if len(unwired) > 0 {
		quoted := []string{}
		for _, name := range names(unwired) {
			quoted = append(quoted, strconv.Quote(name))
		}
		return Set{}, fmt.Errorf("modules.off names %s: turning these modules off is not supported yet; remove them from modules.off", strings.Join(quoted, ", "))
	}
	return set, nil
}

// lookup returns the named module; names are compiled in, so an unknown one
// is a programming error.
func lookup(name string) Module {
	module, ok := find(name)
	if !ok {
		panic("unknown module " + name)
	}
	return module
}

// Equal reports whether two sets run the same modules.
func (s Set) Equal(other Set) bool {
	for _, module := range all {
		if s.off[module.Name] != other.off[module.Name] {
			return false
		}
	}
	return true
}

// Enabled reports whether the set runs the named module.
func (s Set) Enabled(name string) bool {
	lookup(name)
	return !s.off[name]
}

// Check returns nil when the set runs the named module, and otherwise an
// error that names it and the setting that turned it off.
func (s Set) Check(name string) error {
	module := lookup(name)
	if !s.off[name] {
		return nil
	}
	return fmt.Errorf("module %q (%s) is turned off by modules.off in instance.json", name, module.Title)
}

// State is a module and whether a set runs it.
type State struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Enabled bool   `json:"enabled"`
}

// States lists every module, in the registry's order, with whether the set
// runs it.
func (s Set) States() []State {
	out := make([]State, 0, len(all))
	for _, module := range all {
		out = append(out, State{Name: module.Name, Title: module.Title, Enabled: !s.off[module.Name]})
	}
	return out
}

// String names the modules the set runs and those it turns off.
func (s Set) String() string {
	var on, off []string
	for _, module := range all {
		if s.off[module.Name] {
			off = append(off, module.Name)
		} else {
			on = append(on, module.Name)
		}
	}
	list := func(names []string) string {
		if len(names) == 0 {
			return "none"
		}
		return strings.Join(names, ", ")
	}
	return "on: " + list(on) + "; off: " + list(off)
}

func names(modules []Module) []string {
	out := make([]string, 0, len(modules))
	for _, module := range modules {
		out = append(out, module.Name)
	}
	return out
}
