package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func packageBytes(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, data := range parts {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestUnsafeDocumentsRejected(t *testing.T) {
	for name, parts := range map[string]map[string]string{
		"traversal":     {"../word/document.xml": "<document/>"},
		"external main": {"_rels/.rels": `<Relationships><Relationship Id="a" Type="x/officeDocument" TargetMode="External" Target="https://example.com/doc.xml"/></Relationships>`},
		"DTD":           {"word/document.xml": `<!DOCTYPE document [<!ENTITY x SYSTEM "file:///etc/passwd">]><document><p><t>&x;</t></p></document>`},
		"wrong type":    {"word/document.xml": "<workbook/>"},
		"invalid XML":   {"word/document.xml": "<document><broken>"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Prepare(context.Background(), "bad.docx", "", packageBytes(t, parts)); err == nil {
				t.Fatal("unsafe document accepted")
			}
		})
	}
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for i := 0; i < 2; i++ {
		f, _ := w.Create("word/document.xml")
		f.Write([]byte("<document/>"))
	}
	w.Close()
	if _, err := openPackage(out.Bytes()); err == nil {
		t.Fatal("duplicate ZIP accepted")
	}
	out.Reset()
	w = zip.NewWriter(&out)
	h := &zip.FileHeader{Name: "word/document.xml"}
	h.SetMode(os.ModeSymlink | 0777)
	f, _ := w.CreateHeader(h)
	f.Write([]byte("/etc/passwd"))
	w.Close()
	if _, err := openPackage(out.Bytes()); err == nil {
		t.Fatal("symlink ZIP accepted")
	}
	t.Setenv("FMN_LIMIT_IMPORT_BYTES", "33554432")
	huge := packageBytes(t, map[string]string{"word/document.xml": strings.Repeat("x", maxPackageBytes()+1)})
	if _, err := openPackage(huge); err == nil {
		t.Fatal("ZIP expansion limit ignored")
	}
	for _, ref := range []string{"../../secret", "https://example.com/x", "//example.com/x", "a%00b", "../.."} {
		if _, err := partTarget("word/document.xml", ref); err == nil {
			t.Fatal("unsafe reference", ref)
		}
	}
}
func TestDelimitedTextBoundaries(t *testing.T) {
	text, err := delimitedText([]byte("name,quote\nAmber,\"first\nsecond\"\n=1+2\n"), ',')
	if err != nil || !strings.Contains(text, "first<br>second") || !strings.Contains(text, "=1+2") {
		t.Fatal(text, err)
	}
	text, err = plainText([]byte{0xff, 0xfe, 'A', 0, 'm', 0, 'b', 0, 'e', 0, 'r', 0})
	if err != nil || text != "Amber" {
		t.Fatal("UTF-16", text, err)
	}
	for _, raw := range [][]byte{{0xff, 0xfe, 0, 0xd8}, {0xff}, {0, 'a'}} {
		if _, err := plainText(raw); err == nil {
			t.Fatal("invalid text accepted")
		}
	}
	if _, err := delimitedText([]byte(strings.Repeat("a,", maxTableColumns())+"a"), ','); err == nil {
		t.Fatal("column limit ignored")
	}
	if _, err := delimitedText([]byte(strings.Repeat("a\n", maxTableRows()+1)), ','); err == nil {
		t.Fatal("row limit ignored")
	}
	if _, err := delimitedText([]byte("\"unclosed"), ','); err == nil {
		t.Fatal("broken quoting accepted")
	}
}
func TestXLSXAndEPUBInvalidReferences(t *testing.T) {
	for _, test := range []struct{ ext, part, old, new string }{
		{"xlsx", "xl/worksheets/sheet1.xml", `t="s"><v>0</v>`, `t="s"><v>9999</v>`},
		{"xlsx", "xl/worksheets/sheet1.xml", `r="A1"`, `r="ZZZ1"`},
		{"epub", "Book/package.opf", `href="first.xhtml"`, `href="https://example.com/a.xhtml"`},
	} {
		t.Run(test.ext+test.old, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/reference." + test.ext)
			if err != nil {
				t.Fatal(err)
			}
			p, err := openPackage(raw)
			if err != nil {
				t.Fatal(err)
			}
			parts := map[string]string{}
			for name := range p.files {
				data, err := p.read(name)
				if err != nil {
					t.Fatal(err)
				}
				parts[name] = string(data)
			}
			if !strings.Contains(parts[test.part], test.old) {
				t.Fatal("fixture does not contain replacement", test.part)
			}
			parts[test.part] = strings.Replace(parts[test.part], test.old, test.new, 1)
			if _, err := Prepare(context.Background(), "bad."+test.ext, "", packageBytes(t, parts)); err == nil {
				t.Fatal("invalid reference accepted")
			}
		})
	}
}

func TestEncryptedEPUBRejected(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.epub")
	if err != nil {
		t.Fatal(err)
	}
	p, err := openPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for name := range p.files {
		data, err := p.read(name)
		if err != nil {
			t.Fatal(err)
		}
		parts[name] = string(data)
	}
	parts["META-INF/encryption.xml"] = `<encryption><EncryptedData><CipherData><CipherReference URI="Book/first.xhtml"/></CipherData></EncryptedData></encryption>`
	if _, err := Prepare(context.Background(), "locked.epub", "", packageBytes(t, parts)); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatal("encrypted chapter accepted", err)
	}
}
func TestSharedSpreadsheetTextExpansionBounded(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	p, err := openPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for name := range p.files {
		data, err := p.read(name)
		if err != nil {
			t.Fatal(err)
		}
		parts[name] = string(data)
	}
	parts["xl/sharedStrings.xml"] = "<sst><si><t>" + strings.Repeat("a", 2<<20) + "</t></si></sst>"
	parts["xl/worksheets/sheet1.xml"] = `<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>0</v></c><c r="C1" t="s"><v>0</v></c></row></sheetData></worksheet>`
	if _, err := Prepare(context.Background(), "expanded.xlsx", "", packageBytes(t, parts)); err == nil {
		t.Fatal("repeated shared text exceeded output budget")
	}
}
