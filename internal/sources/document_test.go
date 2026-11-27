package sources

import (
	"context"
	"testing"
)

func TestNativeDocumentPackagesRejectTraversalAndDTD(t *testing.T) {
	for _, parts := range []map[string]string{
		{"../outside": "unsafe"},
		{"content.xml": `<!DOCTYPE document [<!ENTITY external SYSTEM "file:///etc/passwd">]><document>&external;</document>`},
	} {
		for _, ext := range []string{"pptx", "odt", "ods", "odp"} {
			if _, err := Prepare(context.Background(), "unsafe."+ext, "", packageBytes(t, parts)); err == nil {
				t.Fatal("unsafe package accepted", ext)
			}
		}
	}
}

func TestOpenDocumentRepetitionsAreBoundedBeforeConversion(t *testing.T) {
	ordinary := packageBytes(t, map[string]string{"content.xml": `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"><office:body><office:spreadsheet><table:table xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"><table:table-column table:number-columns-repeated="3"/><table:table-row table:number-rows-repeated="2"><table:table-cell table:number-columns-repeated="3"/></table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`})
	if _, err := Prepare(context.Background(), "ordinary.ods", "", ordinary); err != nil {
		t.Fatal("ordinary repeated rows and columns were rejected", err)
	}
	for _, row := range []string{
		`<table:table-row table:number-rows-repeated="100000000"><table:table-cell/></table:table-row>`,
		`<table:table-row><table:table-cell table:number-columns-repeated="100000000"/></table:table-row>`,
		`<table:table-row table:number-rows-repeated="1000"><table:table-cell table:number-columns-repeated="128"/></table:table-row>`,
	} {
		raw := packageBytes(t, map[string]string{"content.xml": `<table:table xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0">` + row + `</table:table>`})
		if _, err := Prepare(context.Background(), "expanded.ods", "", raw); err == nil {
			t.Fatal("unbounded repeated cells reached the converter")
		}
	}
}
