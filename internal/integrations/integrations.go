// Package integrations lists the optional integrations a build includes.
//
// Every integration is built in by default, and the release image includes
// them all. A self-hosted build leaves one out with the Go build tag
// fmn_omit_NAME (the Dockerfile's FMN_OMIT build argument sets those tags and
// leaves out the system packages they need). The integration's code sits in
// its owner's files that carry the tag "!fmn_omit_NAME"; a file with the tag
// "fmn_omit_NAME" beside them replaces it with refusals. Searching for the tag
// finds both.
package integrations

import "fmt"

type Integration struct {
	// Name is the integration's build tag suffix and its FMN_OMIT word.
	Name  string `json:"name"`
	Title string `json:"title"`
}

var all = []Integration{
	{Name: "postgres", Title: "PostgreSQL data connections"},
	{Name: "redshift", Title: "Amazon Redshift data connections"},
	{Name: "outlook", Title: "Outlook message sources (.msg)"},
	{Name: "iwork", Title: "Pages and Keynote sources (.pages, .key)"},
	{Name: "ocr", Title: "Tesseract OCR of images and scanned PDF pages"},
	{Name: "slack", Title: "Slack workspace app and personal Slack connections"},
}

// omitted is filled by the init functions of the fmn_omit_NAME files.
var omitted = map[string]bool{}

// All returns every optional integration, whether or not this build includes it.
func All() []Integration { return append([]Integration(nil), all...) }

// Omitted returns the integrations this build leaves out.
func Omitted() []Integration {
	out := []Integration{}
	for _, integration := range all {
		if omitted[integration.Name] {
			out = append(out, integration)
		}
	}
	return out
}

func find(name string) Integration {
	for _, integration := range all {
		if integration.Name == name {
			return integration
		}
	}
	// Names are compiled in; an unknown one is a programming error.
	panic("unknown integration " + name)
}

// Included reports whether this build includes the named integration.
func Included(name string) bool {
	find(name)
	return !omitted[name]
}

// Check returns nil when this build includes the named integration, and
// otherwise an error that names it, for configuration that asks for it.
func Check(name string) error {
	integration := find(name)
	if !omitted[name] {
		return nil
	}
	return fmt.Errorf("this build leaves out %s (built with the tag fmn_omit_%s); the release image includes it", integration.Title, name)
}
