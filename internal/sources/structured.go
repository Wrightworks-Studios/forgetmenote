package sources

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
)

// xmlNode is a bounded document part, not an execution DOM. Ordered character
// nodes preserve words on either side of a span; namespaces stay attached.
type xmlNode struct {
	name     xml.Name
	attrs    []xml.Attr
	value    string
	children []*xmlNode
}

func documentTree(p *documentPackage, part string) (*xmlNode, error) {
	raw, err := p.read(part)
	if err != nil {
		return nil, err
	}
	if err := validateDocumentXML(p.ctx, raw); err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(contextReader{p.ctx, bytes.NewReader(raw)})
	var root *xmlNode
	var stack []*xmlNode
	nodes := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		nodes++
		if nodes > 100000 {
			return nil, errors.New("document XML exceeds 100000 nodes")
		}
		switch token := token.(type) {
		case xml.StartElement:
			n := &xmlNode{name: token.Name, attrs: token.Attr}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			} else {
				if root != nil {
					return nil, errors.New("multiple XML document roots")
				}
				root = n
			}
			stack = append(stack, n)
			if len(stack) > 128 {
				return nil, errors.New("document XML nesting exceeds 128 levels")
			}
		case xml.CharData:
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, &xmlNode{value: string(token)})
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.Directive:
			return nil, errors.New("document XML directives are not supported")
		}
	}
	if root == nil {
		return nil, errors.New("empty document XML")
	}
	return root, nil
}
func (n *xmlNode) attribute(name string) string {
	for _, a := range n.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func (n *xmlNode) find(name string) []*xmlNode {
	var out []*xmlNode
	if n.name.Local == name {
		out = append(out, n)
	}
	for _, child := range n.children {
		out = append(out, child.find(name)...)
	}
	return out
}
func (n *xmlNode) text() string {
	switch n.name.Local {
	case "s":
		count, _ := strconv.Atoi(n.attribute("c"))
		return strings.Repeat(" ", min(1000, max(1, count)))
	case "tab":
		return "\t"
	case "line-break", "br":
		return "\n"
	}
	var out strings.Builder
	out.WriteString(n.value)
	for _, child := range n.children {
		out.WriteString(child.text())
	}
	return out.String()
}
func pptxParagraph(n *xmlNode) string {
	var out strings.Builder
	for _, child := range n.children {
		switch child.name.Local {
		case "t":
			out.WriteString(child.text())
		case "br":
			out.WriteByte('\n')
		default:
			if child.name.Local != "pPr" && child.name.Local != "endParaRPr" {
				out.WriteString(pptxParagraph(child))
			}
		}
	}
	return strings.TrimSpace(out.String())
}
func tableRows(n *xmlNode, odf bool) ([][]string, error) {
	var rows [][]string
	rowName, cellName := "tr", "tc"
	if odf {
		rowName, cellName = "table-row", "table-cell"
	}
	for _, row := range n.find(rowName) {
		var cells []string
		for _, cell := range row.children {
			if cell.name.Local != cellName && !(odf && cell.name.Local == "covered-table-cell") {
				continue
			}
			var paragraphs []string
			for _, p := range cell.find("p") {
				value := p.text()
				if !odf {
					value = pptxParagraph(p)
				}
				paragraphs = append(paragraphs, value)
			}
			value := strings.TrimSpace(strings.Join(paragraphs, "\n"))
			if odf && value == "" {
				for _, key := range []string{"string-value", "value", "boolean-value", "date-value", "time-value"} {
					if saved := cell.attribute(key); saved != "" {
						value = saved
						break
					}
				}
			}
			count := 1
			if odf && cell.attribute("number-columns-repeated") != "" {
				count, _ = strconv.Atoi(cell.attribute("number-columns-repeated"))
			}
			if count < 1 || len(cells)+count > maxTableColumns() {
				return nil, errors.New("document table exceeds column budget")
			}
			for range count {
				cells = append(cells, value)
			}
		}
		count := 1
		if odf && row.attribute("number-rows-repeated") != "" {
			count, _ = strconv.Atoi(row.attribute("number-rows-repeated"))
		}
		if count < 1 || len(rows)+count > maxTableRows() || (len(rows)+count)*len(cells) > maxTableCells() {
			return nil, errors.New("document table exceeds cell budget")
		}
		for range count {
			rows = append(rows, cells)
		}
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	for i, row := range rows {
		for len(row) < columns {
			row = append(row, "")
		}
		rows[i] = row
	}
	return rows, nil
}
func structuredBlocks(n *xmlNode, odf bool) ([]documentBlock, error) {
	switch n.name.Local {
	case "tbl", "table":
		rows, err := tableRows(n, odf)
		if err != nil {
			return nil, err
		}
		return []documentBlock{{Kind: "table", Rows: rows}}, nil
	case "p", "h":
		text := n.text()
		kind := "text"
		level := 1
		if !odf {
			text = pptxParagraph(n)
			for _, pr := range n.find("pPr") {
				if len(pr.find("buChar")) > 0 || len(pr.find("buAutoNum")) > 0 {
					kind = "list"
				}
			}
		}
		if n.name.Local == "h" {
			kind = "heading"
			level, _ = strconv.Atoi(n.attribute("outline-level"))
		}
		return []documentBlock{{Kind: kind, Text: text, Level: level}}, nil
	case "annotation", "tracked-changes", "scripts", "binary-data":
		return nil, nil
	}
	var blocks []documentBlock
	for _, child := range n.children {
		found, err := structuredBlocks(child, odf)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, found...)
	}
	return blocks, nil
}
func pptxResult(p *documentPackage) (documentResult, error) {
	result := documentResult{Extractor: "ForgetMeNote native PPTX XML v1", Unit: "slide"}
	part, err := p.officePart("ppt/presentation.xml")
	if err != nil {
		return result, err
	}
	root, err := documentTree(p, part)
	if err != nil {
		return result, err
	}
	if root.name.Local != "presentation" {
		return result, errors.New("invalid PowerPoint presentation part")
	}
	links, err := p.relationships(part)
	if err != nil {
		return result, err
	}
	for _, slide := range root.find("sldId") {
		id := ""
		for _, a := range slide.attrs {
			if a.Name.Local == "id" && strings.HasSuffix(a.Name.Space, "/relationships") {
				id = a.Value
			}
		}
		link, ok := links[id]
		if !ok || link.Mode == "External" {
			return result, errors.New("missing or external PowerPoint slide relationship")
		}
		target, err := partTarget(part, link.Target)
		if err != nil {
			return result, err
		}
		tree, err := documentTree(p, target)
		if err != nil {
			return result, err
		}
		if tree.name.Local != "sld" {
			return result, errors.New("invalid PowerPoint slide part")
		}
		blocks, err := structuredBlocks(tree, false)
		if err != nil {
			return result, err
		}
		result.Sections = append(result.Sections, documentSection{Number: len(result.Sections) + 1, Blocks: blocks})
	}
	return result, nil
}
func odfResult(p *documentPackage, extension string) (documentResult, error) {
	result := documentResult{Extractor: "ForgetMeNote native OpenDocument XML v1", Unit: "section"}
	root, err := documentTree(p, "content.xml")
	if err != nil {
		return result, err
	}
	body := root.find("body")
	if len(body) != 1 {
		return result, errors.New("OpenDocument content requires one body")
	}
	var groups []*xmlNode
	switch extension {
	case ".odp":
		result.Unit = "slide"
		groups = body[0].find("page")
	case ".ods":
		result.Unit = "sheet"
		groups = body[0].find("table")
	default:
		groups = body
	}
	for _, group := range groups {
		blocks, err := structuredBlocks(group, true)
		if err != nil {
			return result, err
		}
		result.Sections = append(result.Sections, documentSection{Number: len(result.Sections) + 1, Title: group.attribute("name"), Blocks: blocks})
	}
	return result, nil
}
func nativeStructured(extension string, p *documentPackage) (string, string, string, error) {
	var result documentResult
	var err error
	if extension == ".pptx" {
		result, err = pptxResult(p)
	} else {
		result, err = odfResult(p, extension)
	}
	if err != nil {
		return "", "", "", err
	}
	return renderDocumentResult(result)
}
