package sources

import (
	"encoding/binary"
	"path/filepath"

	"os"
	"strings"
	"testing"
)

func TestIWorkAdapterPreservesSectionBoundaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forgetmenote-iwork"), []byte(`#!/bin/sh
case "$1" in
*.key) printf '%s' '{"extractor":"libetonyek fixture","unit":"slide","sections":[{"number":1,"blocks":[{"kind":"text","text":"Amber slide"}]},{"number":2,"blocks":[]},{"number":3,"blocks":[{"kind":"text","text":"Cobalt slide"}]}]}' ;;
*) printf '%s' '{"extractor":"libetonyek fixture","unit":"section","sections":[{"number":1,"blocks":[{"kind":"text","text":"Amber Pages Cobalt evidence [link](javascript:evil)"}]}]}' ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for ext, body := range map[string]string{
		"pages": `<document xmlns:sf="http://developer.apple.com/namespaces/sf"><sf:text-body><sf:p>Amber Pages</sf:p><sf:p>Cobalt evidence [link](javascript:evil)</sf:p></sf:text-body></document>`,
		"key":   `<key:presentation xmlns:key="http://developer.apple.com/namespaces/keynote2" xmlns:sf="http://developer.apple.com/namespaces/sf"><key:slide-list><key:slide><sf:p>Amber slide</sf:p></key:slide><key:slide/><key:slide><sf:p>Cobalt slide</sf:p></key:slide></key:slide-list></key:presentation>`,
	} {
		t.Run(ext, func(t *testing.T) {
			part := "index.xml"
			if ext == "key" {
				part = "index.apxl"
			}
			raw := packageBytes(t, map[string]string{part: body})
			got, err := Prepare(t.Context(), "source."+ext, "", raw)
			if err != nil || !strings.Contains(got.Text, "Amber") || !strings.Contains(got.Text, "Cobalt") || strings.Contains(got.Text, "[link](") {
				t.Fatalf("legacy Apple import: %q %v", got.Text, err)
			}
			if ext == "key" && !strings.Contains(got.Text, "## Slide 2") {
				t.Fatal("blank slide omitted", got.Text)
			}
		})
	}
}
func TestModernIWorkAdapter(t *testing.T) {
	dir := os.Getenv("FMN_IWORK_TEST_FIXTURES")
	if dir == "" {
		t.Skip("requires libetonyek adapter and upstream synthetic fixture directory")
	}
	for fixture, expected := range map[string]string{"modern.pages": "My hovercraft is full of eels.", "modern.key": "Amber modern Keynote", "legacy.key": "Capitalized title", "blank.key": "", "legacy.pages": ""} {
		raw, err := os.ReadFile(dir + "/" + fixture)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Prepare(t.Context(), fixture, "", raw)
		if err != nil || !strings.Contains(got.Text, expected) || !strings.Contains(got.Record.Extractor, "libetonyek") {
			t.Fatalf("iWork extraction %s: %q %v", fixture, got.Text, err)
		}
		if (fixture == "blank.key" || fixture == "legacy.pages") && got.Record.Extraction != "no-text" {
			t.Fatalf("blank iWork state %s: %s", fixture, got.Record.Extraction)
		}
		if fixture == "modern.key" && !strings.Contains(got.Text, "Cobalt body evidence") {
			t.Fatal("modern Keynote body lost", got.Text)
		}
		if fixture == "legacy.key" && !strings.Contains(got.Text, "## Slide 2") {
			t.Fatal("legacy slide lost", got.Text)
		}
	}

}
func TestIWorkExpansionCannotReachConverter(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	// A Snappy length prefix advertises an enormous expanded block in a tiny ZIP.
	block := binary.AppendUvarint(nil, 1<<31)
	raw := append([]byte{0, byte(len(block)), 0, 0}, block...)
	got, err := Prepare(t.Context(), "bad.pages", "", packageBytes(t, map[string]string{"Index/Document.iwa": string(raw)}))
	if err == nil || !strings.Contains(err.Error(), "IWA") {
		t.Fatalf("IWA expansion not rejected at local boundary: %q %v", got.Text, err)
	}
}
