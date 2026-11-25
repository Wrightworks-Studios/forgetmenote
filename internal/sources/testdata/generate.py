"""Generate small, synthetic document fixtures using only ZIP and XML."""
from pathlib import Path
from zipfile import ZipFile, ZipInfo, ZIP_DEFLATED
root = Path(__file__).parent

def package(name, entries):
    with ZipFile(root / name, 'w') as archive:
        for name, data in entries.items():
            info = ZipInfo(name)
            info.compress_type = ZIP_DEFLATED
            archive.writestr(info, data)

package('reference.docx', {
    '[Content_Types].xml': '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/></Types>',
    '_rels/.rels': '<Relationships><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>',
    'word/document.xml': '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Amber observatory</w:t></w:r></w:p><w:p><w:r><w:t>Measured Ægir at 14 grams.</w:t></w:r><w:del><w:r><w:delText>discarded secret</w:delText></w:r></w:del><w:r><w:instrText>DO NOT EXECUTE</w:instrText></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>Sample</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Mass</w:t></w:r></w:p></w:tc></w:tr><w:tr><w:tc><w:p><w:r><w:t>Amber</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>14 g</w:t></w:r></w:p></w:tc></w:tr></w:tbl><w:sectPr/></w:body></w:document>'})
package('reference.epub', {
    'mimetype': 'application/epub+zip',
    'META-INF/container.xml': '<container><rootfiles><rootfile full-path="Book/package.opf" media-type="application/oebps-package+xml"/></rootfiles></container>',
    'Book/package.opf': '<package><metadata><title>Observatory field book</title></metadata><manifest><item id="b" href="second.xhtml" media-type="application/xhtml+xml"/><item id="a" href="first.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/><itemref idref="b"/></spine></package>',
    'Book/second.xhtml': '<html><head><title>Later evidence</title></head><body><h1>Cobalt</h1><p>Second chapter evidence.</p></body></html>',
    'Book/first.xhtml': '<html><head><title>First observation</title></head><body><h1>Amber</h1><p>First chapter evidence.</p><script>discarded secret</script></body></html>'})
package('reference.xlsx', {
    '[Content_Types].xml': '<Types/>',
    '_rels/.rels': '<Relationships><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>',
    'xl/workbook.xml': '<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Observations" sheetId="1" r:id="r1"/><sheet name="Supplement" sheetId="2" r:id="r2" state="hidden"/></sheets></workbook>',
    'xl/_rels/workbook.xml.rels': '<Relationships><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="r2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/><Relationship Id="s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/></Relationships>',
    'xl/sharedStrings.xml': '<sst><si><t>Amber sample</t></si><si><r><t>Rich </t></r><r><t>text</t></r></si></sst>',
    'xl/worksheets/sheet1.xml': '<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1"><v>14</v></c></row><row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2"><f>7*2</f><v>14</v></c></row><row r="4"><c r="A4" t="inlineStr"><is><t>Cobalt</t></is></c><c r="B4"><f>1+2</f></c><c r="C4" t="b"><v>1</v></c></row></sheetData></worksheet>',
    'xl/worksheets/sheet2.xml': '<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Supplementary evidence</t></is></c></row></sheetData></worksheet>'})
(root/'reference.txt').write_text('Amber observatory\n\nMeasured Ægir at 14 grams.\n', encoding='utf-8')
(root/'reference.csv').write_text('Sample,Mass,Comment\nAmber,14,"First line\nSecond line"\nCobalt,12,"=1+2"\n', encoding='utf-8')
(root/'reference.tsv').write_text('Sample\tMass\nAmber\t14\n', encoding='utf-8')
