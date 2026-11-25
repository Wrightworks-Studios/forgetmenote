package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"forgetmenote/internal/workspace"
	"golang.org/x/net/html/charset"
)

type emailPart struct {
	text        string
	attachments []string
}

func emailText(ctx context.Context, raw []byte) (string, error) {
	message, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return "", errors.New("invalid email message")
	}
	var out boundedText
	words := &mime.WordDecoder{CharsetReader: func(label string, reader io.Reader) (io.Reader, error) { return charset.NewReaderLabel(label, reader) }}
	for _, key := range []string{"Subject", "From", "To", "Cc", "Date"} {
		value := message.Header.Get(key)
		if value == "" {
			continue
		}
		value, err = words.DecodeHeader(value)
		if err != nil {
			return "", errors.New("invalid encoded email header")
		}
		if _, err := out.Write([]byte("**" + key + ":** " + workspace.EscapeSourceText(value) + "\n\n")); err != nil {
			return "", err
		}
	}
	count := 0
	body, err := readEmailPart(ctx, textproto.MIMEHeader(message.Header), message.Body, 0, &count)
	if err != nil {
		return "", err
	}
	if _, err := out.Write([]byte(body.text)); err != nil {
		return "", err
	}
	if len(body.attachments) > 0 {
		if _, err := out.Write([]byte("\n\nAttachments (not opened): " + workspace.EscapeSourceText(strings.Join(body.attachments, ", ")))); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}
func readEmailPart(ctx context.Context, header textproto.MIMEHeader, reader io.Reader, depth int, count *int) (emailPart, error) {
	if err := ctx.Err(); err != nil {
		return emailPart{}, err
	}
	*count++
	if depth > 16 || *count > 1000 {
		return emailPart{}, errors.New("email exceeds nesting or part budget")
	}
	kind, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if header.Get("Content-Type") == "" {
		kind = "text/plain"
		params = map[string]string{}
		err = nil
	}
	if err != nil {
		return emailPart{}, errors.New("invalid email content type")
	}
	disposition, dparams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	if disposition == "attachment" || filename != "" {
		if filename == "" {
			filename = "unnamed attachment"
		}
		return emailPart{attachments: []string{filename}}, nil
	}
	switch strings.ToLower(header.Get("Content-Transfer-Encoding")) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, reader)
	case "quoted-printable":
		reader = quotedprintable.NewReader(reader)
	case "", "7bit", "8bit", "binary":
	default:
		return emailPart{}, errors.New("unsupported email transfer encoding")
	}
	if strings.HasPrefix(kind, "multipart/") {
		if params["boundary"] == "" {
			return emailPart{}, errors.New("email multipart has no boundary")
		}
		multipartReader := multipart.NewReader(contextReader{ctx, reader}, params["boundary"])
		var out emailPart
		chosen := false
		for {
			part, err := multipartReader.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out, errors.New("invalid email MIME parts")
			}
			found, err := readEmailPart(ctx, part.Header, part, depth+1, count)
			part.Close()
			if err != nil {
				return out, err
			}
			out.attachments = append(out.attachments, found.attachments...)
			if kind == "multipart/alternative" {
				if !chosen && found.text != "" {
					out.text = found.text
					chosen = true
				}
			} else {
				out.text += found.text + "\n\n"
			}
			if len(out.text) > workspace.MaxSourceText() {
				return out, errors.New("email text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
			}
		}
		return out, nil
	}
	if kind != "text/plain" && kind != "text/html" {
		return emailPart{}, nil
	}
	label := strings.ToLower(params["charset"])
	if label != "" && label != "utf-8" && label != "us-ascii" {
		reader, err = charset.NewReaderLabel(label, reader)
		if err != nil {
			return emailPart{}, errors.New("unsupported email character encoding")
		}
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, int64(workspace.MaxSourceText())+1))
	if err != nil || len(raw) > workspace.MaxSourceText() {
		return emailPart{}, errors.New("email part exceeds FMN_LIMIT_SOURCE_TEXT_BYTES or is invalid")
	}
	if kind == "text/html" {
		_, text, err := htmlTextContext(ctx, raw, kind)
		return emailPart{text: text}, err
	}
	text, err := plainText(raw)
	return emailPart{text: workspace.EscapeSourceText(text)}, err
}
