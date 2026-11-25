package messaging

// Messages, cards and replies that every build validates and lays out. The
// Slack Web API transport that sends them is optional (slack.go); a build
// with the tag fmn_omit_slack leaves it out (slack_omitted.go).

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // card picture checks
	_ "image/png"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrNotPosted marks a failure where nothing reached the channel: the request
// was never sent, or Slack answered with a definitive refusal. Any other
// failure after sending leaves the outcome unknown.
var ErrNotPosted = errors.New("Slack message not posted")

type notPosted struct{ error }

func (e notPosted) Unwrap() error      { return e.error }
func (notPosted) Is(target error) bool { return target == ErrNotPosted }

// NotPosted marks err as a failure that sent nothing to Slack.
func NotPosted(err error) error { return notPosted{err} }

// buttonBlocks lays out sections and buttons. A section's one button sits
// beside its text; several go in a row below it.
func buttonBlocks(sections []Section, buttons []Button) []map[string]any {
	element := func(b Button) map[string]any {
		button := map[string]any{"type": "button", "action_id": b.Action, "text": map[string]string{"type": "plain_text", "text": b.Label}, "value": b.Value}
		if b.Style != "" {
			button["style"] = b.Style
		}
		return button
	}
	row := func(buttons []Button) map[string]any {
		elements := []map[string]any{}
		for _, b := range buttons {
			elements = append(elements, element(b))
		}
		return map[string]any{"type": "actions", "elements": elements}
	}
	blocks := []map[string]any{}
	for _, section := range sections {
		block := map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": escapeMentions(section.Text)}}
		blocks = append(blocks, block)
		switch len(section.Buttons) {
		case 0:
		case 1:
			block["accessory"] = element(section.Buttons[0])
		default:
			blocks = append(blocks, row(section.Buttons))
		}
	}
	if len(buttons) > 0 {
		blocks = append(blocks, row(buttons))
	}
	return blocks
}

// SlackCommandReply answers a slash command with a reply, its sections and
// buttons, that only the member who typed the command sees. Slack shows one
// message, so text beyond it ends with more.
func SlackCommandReply(reply Reply, more string) map[string]any {
	text := ""
	if parts := messageParts(escapeMentions(reply.Text), messageLimit); len(parts) > 1 {
		text = parts[0] + "\n\n" + more
	} else if len(parts) == 1 {
		text = parts[0]
	}
	blocks := append([]map[string]any{{"type": "markdown", "text": text}}, buttonBlocks(reply.Sections, reply.Buttons)...)
	return map[string]any{"response_type": "ephemeral", "text": text, "blocks": blocks}
}

// messageLimit is the most characters of text sent in one Slack message.
const messageLimit = 3500

// Source content and model output must not notify users or groups merely by
// containing Slack's special mention syntax, including file comments.
func escapeMentions(text string) string {
	return strings.NewReplacer("<!", "&lt;!", "<@", "&lt;@").Replace(text)
}

var messageTS = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,8}$`)

// Post is one channel message. Thread is the ID of an earlier message to
// reply under.
type Post struct {
	Text   string
	Thread string
	Image  *Image
	// Images shows several pictures in one message, in order; use it or Image.
	Images []Image
	// Cards shows one carousel after the text, instead of Image or Images.
	Cards []Card
}

// maxPostImages bounds one message's pictures; each is a separate image block.
const maxPostImages = 4

// maxPostCards is Slack's limit on one carousel's cards; maxCardImageBytes
// bounds one card picture the host uploads.
const (
	maxPostCards      = 10
	maxCardImageBytes = 2 << 20
)

// Card is one carousel card. Slack frames a card's picture at 4:3 and crops
// any other shape, so callers send pictures already padded to it.
type Card struct {
	Title    string     `json:"title,omitempty"`
	Subtitle string     `json:"subtitle,omitempty"`
	Body     string     `json:"body,omitempty"`
	Image    *CardImage `json:"image,omitempty"`
}

// CardImage is a public HTTPS URL Slack fetches, or Data: a PNG or JPEG data
// URI the host uploads to Slack privately, so the picture needs no public
// address and is never posted as a file of its own.
type CardImage struct {
	URL  string `json:"url,omitempty"`
	Data string `json:"data,omitempty"`
	Alt  string `json:"alt"`
}

var cardImageTypes = []struct{ prefix, format, ext string }{
	{"data:image/png;base64,", "png", "png"},
	{"data:image/jpeg;base64,", "jpeg", "jpg"},
}

// decoded returns a data picture's bytes and file extension, checking the
// bytes are the declared PNG or JPEG.
func (c CardImage) decoded() ([]byte, string, error) {
	for _, kind := range cardImageTypes {
		encoded, ok := strings.CutPrefix(c.Data, kind.prefix)
		if !ok {
			continue
		}
		if base64.StdEncoding.DecodedLen(len(encoded)) > maxCardImageBytes+2 {
			return nil, "", fmt.Errorf("a card image is at most %d bytes", maxCardImageBytes)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) > maxCardImageBytes {
			return nil, "", fmt.Errorf("a card image must be base64 of at most %d bytes", maxCardImageBytes)
		}
		if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != kind.format {
			return nil, "", errors.New("card image data is not the PNG or JPEG it declares")
		}
		return data, kind.ext, nil
	}
	return nil, "", errors.New("card image data must be a PNG or JPEG data URI")
}

func validImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && len(raw) <= 3000
}

func validAlt(alt string) bool {
	return strings.TrimSpace(alt) != "" && utf8.RuneCountInString(alt) <= 2000
}

func (c Card) validate() error {
	for _, field := range []struct {
		name, text string
		limit      int
	}{{"title", c.Title, 150}, {"subtitle", c.Subtitle, 150}, {"body", c.Body, 200}} {
		if utf8.RuneCountInString(field.text) > field.limit {
			return fmt.Errorf("a card %s is at most %d characters", field.name, field.limit)
		}
	}
	if strings.TrimSpace(c.Title) == "" && c.Image == nil {
		return errors.New("a card needs a title or an image")
	}
	if c.Image == nil {
		return nil
	}
	if !validAlt(c.Image.Alt) {
		return errors.New("a card image needs alt text of 1–2000 characters")
	}
	if (c.Image.URL == "") == (c.Image.Data == "") {
		return errors.New("a card image needs a url or data, not both")
	}
	if c.Image.URL != "" && !validImageURL(c.Image.URL) {
		return errors.New("a card image url must be HTTPS and at most 3000 characters")
	}
	if c.Image.Data != "" {
		_, _, err := c.Image.decoded()
		return err
	}
	return nil
}

func (p Post) images() []Image {
	if p.Image != nil {
		return []Image{*p.Image}
	}
	return p.Images
}

// Image is shown from its public URL; Slack fetches it when the message posts.
type Image struct {
	URL string `json:"url"`
	Alt string `json:"alt"`
}

// Validate rejects a post before anything is sent. Text is never split, so a
// post yields exactly one message ID.
func (p Post) Validate() error {
	if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(escapeMentions(p.Text)) > messageLimit {
		return fmt.Errorf("Slack message text must be 1–%d characters", messageLimit)
	}
	if p.Thread != "" && !messageTS.MatchString(p.Thread) {
		return errors.New("thread must be a Slack message ID returned by an earlier post")
	}
	if p.Image != nil && len(p.Images) > 0 {
		return errors.New("use image or images, not both")
	}
	if len(p.Images) > maxPostImages {
		return fmt.Errorf("a message shows at most %d images", maxPostImages)
	}
	for _, image := range p.images() {
		if !validImageURL(image.URL) {
			return errors.New("image needs an HTTPS URL of at most 3000 characters")
		}
		if !validAlt(image.Alt) {
			return errors.New("image needs alt text of 1–2000 characters")
		}
	}
	if len(p.Cards) > 0 && len(p.images()) > 0 {
		return errors.New("use cards or images, not both")
	}
	if len(p.Cards) > maxPostCards {
		return fmt.Errorf("a carousel shows at most %d cards", maxPostCards)
	}
	for _, card := range p.Cards {
		if err := card.validate(); err != nil {
			return err
		}
	}
	return nil
}

func messageParts(text string, limit int) []string {
	chars := []rune(text)
	parts := []string{}
	for len(chars) > 0 {
		n := min(len(chars), limit)
		parts = append(parts, string(chars[:n]))
		chars = chars[n:]
	}
	return parts
}
