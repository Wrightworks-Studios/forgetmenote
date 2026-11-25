//go:build !fmn_omit_iwork

package sources

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"forgetmenote/internal/workspace"
	"github.com/golang/snappy"
)

func preflightIWork(p *documentPackage, total *int, depth int) error {
	if depth > 16 {
		return errors.New("iWork ZIP nesting exceeds budget")
	}
	if nested := p.files["Index.zip"]; nested != nil {
		raw, err := p.read("Index.zip")
		if err != nil {
			return err
		}
		index, err := openPackage(raw)
		if err != nil {
			return err
		}
		index.ctx = p.ctx
		*total += len(raw)
		if *total > maxPackageBytes() {
			return errors.New("iWork expansion exceeds package budget")
		}
		if err := preflightIWork(index, total, depth+1); err != nil {
			return err
		}
	}
	for name := range p.files {
		if strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".apxl") {
			raw, err := p.read(name)
			if err != nil {
				return err
			}
			*total += len(raw)
			if *total > maxPackageBytes() {
				return errors.New("iWork expansion exceeds package budget")
			}
			if err := validateDocumentXML(p.ctx, raw); err != nil {
				return err
			}
		}
		if strings.HasSuffix(name, ".xml.gz") || strings.HasSuffix(name, ".apxl.gz") {
			raw, err := p.read(name)
			if err != nil {
				return err
			}
			reader, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return errors.New("invalid compressed iWork XML")
			}
			decoded, err := io.ReadAll(io.LimitReader(contextReader{p.ctx, reader}, int64(maxPartBytes())+1))
			reader.Close()
			if err != nil || len(decoded) > maxPartBytes() {
				return errors.New("iWork XML expansion exceeds part budget")
			}
			*total += len(decoded)
			if *total > maxPackageBytes() {
				return errors.New("iWork expansion exceeds package budget")
			}
			if err := validateDocumentXML(p.ctx, decoded); err != nil {
				return err
			}
		}
		if !strings.HasSuffix(name, ".iwa") {
			continue
		}
		raw, err := p.read(name)
		if err != nil {
			return err
		}
		for len(raw) > 0 {
			if err := p.ctx.Err(); err != nil {
				return err
			}
			if len(raw) < 4 || raw[0] != 0 {
				return errors.New("invalid IWA compression header")
			}
			length := int(raw[1]) | int(raw[2])<<8 | int(raw[3])<<16
			raw = raw[4:]
			if length == 0 || length > len(raw) {
				return errors.New("invalid IWA compressed block length")
			}
			block := raw[:length]
			size, err := snappy.DecodedLen(block)
			if err != nil || size > maxPartBytes() || *total > maxPackageBytes()-size {
				return errors.New("IWA expansion exceeds source part/package budget")
			}
			// Decode checks actual run lengths as well as the advertised size before
			// passing the source to the native library's independently bounded process.
			if _, err := snappy.Decode(nil, block); err != nil {
				return errors.New("invalid IWA compressed data")
			}
			*total += size
			raw = raw[length:]
		}
	}
	return nil
}
func extractIWork(ctx context.Context, extension string, raw []byte) (string, string, string, error) {
	p, err := openPackage(raw)
	if err != nil {
		return "", "", "", refused(err)
	}
	p.ctx = ctx
	total := 0
	if err := preflightIWork(p, &total, 0); err != nil {
		return "", "", "", refused(err)
	}
	select {
	case documentWork <- struct{}{}:
		defer func() { <-documentWork }()
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	}
	dir, err := scratchDirectory(ctx, "forgetmenote-iwork-")
	if err != nil {
		return "", "", "", err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "document"+extension)
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		return "", "", "", err
	}
	output, err := sourceCommand(ctx, dir, workspace.MaxSourceText()*4, "forgetmenote-iwork", filename)
	if errors.As(err, new(toolFailure)) {
		return "", "", "", rejection("iWork conversion failed; use a valid unencrypted Pages/Keynote file")
	}
	if err != nil {
		return "", "", "", errors.New("iWork conversion failed; use a valid unencrypted Pages/Keynote file and install the bundled libetonyek adapter")
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var result documentResult
	if err := decoder.Decode(&result); err != nil {
		return "", "", "", errors.New("invalid iWork conversion result")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", "", "", errors.New("invalid trailing iWork conversion data")
	}
	expected := "section"
	if extension == ".key" {
		expected = "slide"
	}
	if result.Unit != expected {
		return "", "", "", errors.New("invalid iWork section unit")
	}
	return renderDocumentResult(result)
}
