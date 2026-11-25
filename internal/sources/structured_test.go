package sources

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestNativePresentationAndOpenDocumentBoundaries(t *testing.T) {
	// A successful import must work without any external converter.
	t.Setenv("PATH", t.TempDir())
	pptx := packageBytes(t, map[string]string{
		"ppt/presentation.xml":            `<p:presentation xmlns:p="p" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="4" r:id="last"/><p:sldId id="5" r:id="blank"/><p:sldId id="6" r:id="first"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="last" Target="slides/slide3.xml"/><Relationship Id="blank" Target="slides/slide2.xml"/><Relationship Id="first" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide3.xml":           `<sld><spTree><sp><txBody><p><r><t>Amber ordered slide</t></r></p></txBody></sp></spTree></sld>`,
		"ppt/slides/slide2.xml":           `<sld><spTree/></sld>`,
		"ppt/slides/slide1.xml":           `<sld><spTree><graphicFrame><tbl><tr><tc><txBody><p><r><t>Cobalt</t></r></p></txBody></tc><tc><txBody><p><r><t>17</t></r></p></txBody></tc></tr></tbl></graphicFrame></spTree></sld>`,
	})
	got, err := Prepare(t.Context(), "ordered.pptx", "", pptx)
	if err != nil || got.Record.Extraction != "extracted" || !strings.Contains(got.Text, "## Slide 2") || !strings.Contains(got.Text, "| Cobalt | 17 |") || strings.Index(got.Text, "Amber") > strings.Index(got.Text, "Cobalt") {
		t.Fatalf("native PPTX lost physical order, blank slide or table: %+v %v", got, err)
	}
	ns := `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"`
	for ext, content := range map[string]string{
		"odt": `<office:text><text:h text:outline-level="1">Amber title</text:h><text:p>Cobalt <text:span>paragraph</text:span><text:s text:c="2"/>literal [link](javascript:evil)</text:p></office:text>`,
		"ods": `<office:spreadsheet><table:table table:name="Evidence"><table:table-row table:number-rows-repeated="2"><table:table-cell table:number-columns-repeated="2" office:value-type="float" office:value="17"/><table:table-cell office:value-type="string"><text:p>Amber Cobalt</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet>`,
		"odp": `<office:presentation><draw:page><text:p>Amber slide</text:p></draw:page><draw:page/><draw:page><text:p>Cobalt slide</text:p></draw:page></office:presentation>`,
	} {
		t.Run(ext, func(t *testing.T) {
			raw := packageBytes(t, map[string]string{"content.xml": `<office:document-content ` + ns + `><office:body>` + content + `</office:body></office:document-content>`})
			got, err := Prepare(t.Context(), "source."+ext, "", raw)
			if err != nil || !strings.Contains(got.Text, "Amber") || !strings.Contains(got.Text, "Cobalt") || strings.Contains(got.Text, "[link](") {
				t.Fatalf("native %s import lost or activated content: %+v %v", ext, got, err)
			}
			if ext == "ods" && strings.Count(got.Text, "| 17 | 17 | Amber Cobalt |") != 2 {
				t.Fatal("stored/repeated values lost", got.Text)
			}
			if ext == "odp" && !strings.Contains(got.Text, "## Slide 2") {
				t.Fatal("blank slide omitted", got.Text)
			}
		})
	}
}

func TestNativeEmailMIMEIsLiteralAndNeverFetchesAttachments(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	raw := []byte("From: Alice <alice@example.test>\r\nSubject: =?UTF-8?Q?Amber_report?=\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\nQ29iYWx0IGV2aWRlbmNl\r\n--x\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Cobalt evidence</p><script>secret()</script><img src=\"http://127.0.0.1\">\r\n--x--\r\n")
	got, err := Prepare(t.Context(), "message.eml", "", raw)
	if err != nil || !strings.Contains(got.Text, "Amber report") || strings.Count(got.Text, "Cobalt evidence") != 1 || strings.Contains(got.Text, "secret()") || string(got.Original) != string(raw) {
		t.Fatalf("email body/headers unsafe or repeated: %+v %v", got, err)
	}
}

func utf16le(text string) ([]byte, error) {
	units := utf16.Encode([]rune(text))
	raw := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[i*2:], u)
	}
	return raw, nil
}
