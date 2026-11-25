package sources

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"forgetmenote/internal/workspace"
)

func epubText(p *documentPackage) (string, string, error) {
	var container struct {
		Rootfiles []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := p.xml("META-INF/container.xml", &container); err != nil {
		return "", "", err
	}
	if len(container.Rootfiles) == 0 {
		return "", "", errors.New("EPUB package location is missing")
	}
	part, err := partTarget("", container.Rootfiles[0].Path)
	if err != nil {
		return "", "", err
	}
	var book struct {
		XMLName xml.Name `xml:"package"`
		Title   string   `xml:"metadata>title"`
		Items   []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
			MIME string `xml:"media-type,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			ID     string `xml:"idref,attr"`
			Linear string `xml:"linear,attr"`
		} `xml:"spine>itemref"`
	}
	if err = p.xml(part, &book); err != nil {
		return "", "", err
	}
	if len(book.Spine) == 0 || len(book.Spine) > 512 {
		return "", "", errors.New("EPUB needs 1–512 spine entries")
	}
	manifest := map[string]int{}
	for i, item := range book.Items {
		if _, ok := manifest[item.ID]; ok {
			return "", "", errors.New("duplicate EPUB manifest ID")
		}
		manifest[item.ID] = i
	}
	encrypted := map[string]bool{}
	if p.files["META-INF/encryption.xml"] != nil {
		var data struct {
			Items []struct {
				Reference struct {
					URI string `xml:"URI,attr"`
				} `xml:"CipherData>CipherReference"`
			} `xml:"EncryptedData"`
		}
		if err = p.xml("META-INF/encryption.xml", &data); err != nil {
			return "", "", err
		}
		for _, item := range data.Items {
			target, e := partTarget("", item.Reference.URI)
			if e != nil {
				return "", "", e
			}
			encrypted[target] = true
		}
	}
	var out strings.Builder
	for i, entry := range book.Spine {
		index, ok := manifest[entry.ID]
		if !ok {
			return "", "", errors.New("EPUB spine refers to a missing manifest item")
		}
		item := book.Items[index]
		target, err := partTarget(part, item.Href)
		if err != nil {
			return "", "", err
		}
		if encrypted[target] {
			return "", "", errors.New("encrypted EPUB chapters are not supported")
		}
		if item.MIME != "application/xhtml+xml" && item.MIME != "text/html" {
			return "", "", errors.New("EPUB spine contains unsupported non-HTML content")
		}
		raw, err := p.read(target)
		if err != nil {
			return "", "", err
		}
		title, text, err := htmlTextContext(p.ctx, raw, item.MIME)
		if err != nil {
			return "", "", err
		}
		if title == "" {
			title = target
		}
		if entry.Linear == "no" {
			title += " (supplement)"
		}
		fmt.Fprintf(&out, "## Chapter %d — %s\n\n", i+1, workspace.EscapeSourceText(title))
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "###### ") {
				out.WriteByte('#')
			}
			out.WriteString(line + "\n")
		}
		out.WriteByte('\n')
		if out.Len() > workspace.MaxSourceText() {
			return "", "", errors.New("extracted EPUB text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
	}
	return strings.TrimSpace(book.Title), out.String(), nil
}
