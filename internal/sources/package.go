package sources

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"forgetmenote/internal/capacity"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

func maxPackageBytes() int { return capacity.ImportBytes.Limit() }
func maxPartBytes() int    { return capacity.SourceTextBytes.Limit() * 2 }

// ZIP parts are read in memory, never expanded to filesystem paths. Both declared
// and actual sizes are bounded, including media we do not extract.
type documentPackage struct {
	ctx       context.Context
	files     map[string]*zip.File
	readBytes int
}

func openPackage(raw []byte) (*documentPackage, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, errors.New("invalid document ZIP container (encrypted or legacy Office files are not supported)")
	}
	if len(reader.File) > capacity.ImportObjects.Limit() {
		return nil, errors.New("document exceeds FMN_LIMIT_IMPORT_OBJECTS")
	}
	p := &documentPackage{ctx: context.Background(), files: map[string]*zip.File{}}
	var total uint64
	for _, file := range reader.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") || file.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("document contains unsafe ZIP paths")
		}
		if _, ok := p.files[file.Name]; ok {
			return nil, errors.New("document contains duplicate ZIP entries")
		}
		if file.UncompressedSize64 > uint64(maxPackageBytes()) || total > uint64(maxPackageBytes())-file.UncompressedSize64 {
			return nil, errors.New("document expansion exceeds FMN_LIMIT_IMPORT_BYTES")
		}
		total += file.UncompressedSize64
		p.files[file.Name] = file
	}
	return p, nil
}
func (p *documentPackage) read(name string) ([]byte, error) {
	file := p.files[name]
	if file == nil {
		return nil, errors.New("document is missing required part: " + name)
	}
	if file.UncompressedSize64 > uint64(maxPartBytes()) {
		return nil, errors.New("document XML/chapter exceeds twice FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, errors.New("document part cannot be opened")
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{p.ctx, reader}, int64(maxPartBytes())+1))
	if err != nil {
		return nil, errors.New("document part is corrupt or encrypted")
	}
	if len(data) > maxPartBytes() || p.readBytes > maxPackageBytes()-len(data) {
		return nil, errors.New("document extraction exceeds FMN_LIMIT_IMPORT_BYTES")
	}
	p.readBytes += len(data)
	return data, nil
}
func decodeXMLContext(ctx context.Context, data []byte, value any) error {
	decoder := xml.NewDecoder(contextReader{ctx, bytes.NewReader(data)})
	// The standard decoder never fetches entities; reject DTDs explicitly as well.
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid document XML (only UTF-8 XML is supported)")
		}
		if _, ok := token.(xml.Directive); ok {
			return errors.New("document XML directives are not supported")
		}
	}
	if err := xml.NewDecoder(contextReader{ctx, bytes.NewReader(data)}).Decode(value); err != nil {
		return errors.New("invalid document XML structure")
	}
	return nil
}
func (p *documentPackage) xml(name string, value any) error {
	data, err := p.read(name)
	if err != nil {
		return err
	}
	return decodeXMLContext(p.ctx, data, value)
}
func partTarget(base, ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Path == "" || strings.ContainsAny(u.Path, "\\\x00") {
		return "", errors.New("external or invalid document part reference")
	}
	name := path.Clean(path.Join(path.Dir(base), u.Path))
	if strings.HasPrefix(u.Path, "/") {
		name = strings.TrimPrefix(path.Clean(u.Path), "/")
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", errors.New("document part escapes its package")
	}
	return name, nil
}

type relationship struct {
	ID     string `xml:"Id,attr"`
	Type   string `xml:"Type,attr"`
	Target string `xml:"Target,attr"`
	Mode   string `xml:"TargetMode,attr"`
}

func (p *documentPackage) relationships(part string) (map[string]relationship, error) {
	name := "_rels/.rels"
	if part != "" {
		name = path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
	}
	out := map[string]relationship{}
	if p.files[name] == nil {
		return out, nil
	}
	var doc struct {
		Items []relationship `xml:"Relationship"`
	}
	if err := p.xml(name, &doc); err != nil {
		return nil, err
	}
	for _, r := range doc.Items {
		if _, ok := out[r.ID]; ok {
			return nil, errors.New("duplicate document relationship")
		}
		out[r.ID] = r
	}
	return out, nil
}
func (p *documentPackage) officePart(fallback string) (string, error) {
	links, err := p.relationships("")
	if err != nil {
		return "", err
	}
	for _, link := range links {
		if strings.HasSuffix(link.Type, "/officeDocument") {
			if link.Mode == "External" {
				return "", errors.New("external Office document is not supported")
			}
			return partTarget("", link.Target)
		}
	}
	return fallback, nil
}
