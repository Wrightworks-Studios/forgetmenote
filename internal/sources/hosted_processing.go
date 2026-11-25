package sources

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/workspace"
	"image/png"
)

func pageRange(options Options, total int) (int, int, error) {
	first, last := options.FirstPage, options.LastPage
	if first == 0 && last == 0 {
		first, last = 1, total
	}
	if first < 1 || last < first || last > total || last-first+1 > 8 {
		return 0, 0, rejection("choose an explicit valid range of at most eight physical pages/images for hosted processing")
	}
	return first, last, nil
}
func sourceImages(ctx context.Context, extension string, raw []byte, options Options) ([]hostedImage, error) {
	select {
	case documentWork <- struct{}{}:
		defer func() { <-documentWork }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var images []hostedImage
	totalBytes := 0
	add := func(number int, data, header []byte) error {
		decoded, err := decodeImage(ctx, data, header)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return refused(err)
		}
		encoded := procgroup.Output{Limit: 24 << 20, Err: errExtractorOutput}
		if err := png.Encode(&encoded, decoded); err != nil {
			return err
		}
		totalBytes += encoded.Len()
		if totalBytes > 24<<20 {
			return rejection("hosted images exceed 24 MiB upload budget; select fewer pages")
		}
		images = append(images, hostedImage{number: number, data: encoded.Bytes()})
		return nil
	}
	if extension == ".pdf" {
		dir, err := scratchDirectory(ctx, "forgetmenote-vision-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		filename := filepath.Join(dir, "source.pdf")
		if err := os.WriteFile(filename, raw, 0600); err != nil {
			return nil, err
		}
		geometry, err := sourceCommand(ctx, dir, 8*workspace.MaxSourceText()+1<<20, "pdftotext", "-bbox-layout", "-enc", "UTF-8", filename, "-")
		if err != nil {
			return nil, err
		}
		var doc struct {
			Pages []pdfGeometry `xml:"body>doc>page"`
		}
		if xml.Unmarshal(geometry, &doc) != nil || len(doc.Pages) > 10000 {
			return nil, errors.New("invalid PDF physical page inventory")
		}
		first, last, err := pageRange(options, len(doc.Pages))
		if err != nil {
			return nil, err
		}
		for n := first; n <= last; n++ {
			if options.pageStart != nil {
				if err := options.pageStart(n); err != nil {
					return nil, err
				}
			}
			base := filepath.Join(dir, "page")
			if _, err := sourceCommand(ctx, dir, 1024, "pdftoppm", "-f", strconv.Itoa(n), "-l", strconv.Itoa(n), "-r", "300", "-scale-to", "4096", "-singlefile", "-png", filename, base); err != nil {
				return nil, err
			}
			file := base + ".png"
			info, err := os.Stat(file)
			if err != nil || info.Size() > 32<<20 {
				return nil, errors.New("hosted PDF rendering exceeds scratch budget")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			if err := add(n, data, nil); err != nil {
				return nil, err
			}
			os.Remove(file)
		}
	} else if extension == ".pptx" {
		p, err := openPackage(raw)
		if err != nil {
			return nil, err
		}
		p.ctx = ctx
		var parts []string
		for name := range p.files {
			if strings.HasPrefix(name, "ppt/media/") && strings.HasPrefix(workspace.SourceFormatForName(name).MIME, "image/") {
				parts = append(parts, name)
			}
		}
		sort.Strings(parts)
		first, last, err := pageRange(options, len(parts))
		if err != nil {
			return nil, err
		}
		for i := first - 1; i < last; i++ {
			data, err := p.read(parts[i])
			if err != nil {
				return nil, err
			}
			if err := add(i+1, data, nil); err != nil {
				return nil, err
			}
		}
	} else {
		frames, err := imageFrames(raw, extension)
		if err != nil {
			return nil, err
		}
		first, last, err := pageRange(options, len(frames))
		if err != nil {
			return nil, err
		}
		for n := first; n <= last; n++ {
			if err := add(n, raw, frames[n-1]); err != nil {
				return nil, err
			}
		}
	}
	return images, nil
}
func (h *Hosted) Process(ctx context.Context, capture *workspace.SourceCapture, options Options) error {
	format := workspace.SourceFormatForMIME(capture.Record.MIME)
	if len(format.Extensions) == 0 {
		return errors.New("unknown source processing format")
	}
	extension := format.Extensions[0]
	if options.Transcribe && !mediaExtension(extension) {
		return rejection("transcription requires an audio or video source")
	}
	if options.ExternalOCR && !(extension == ".pdf" || extension == ".png" || extension == ".jpg" || extension == ".tiff" || extension == ".webp" || extension == ".bmp") {
		return rejection("external OCR is available for PDF and image sources")
	}
	if options.Transcribe {
		return h.media(ctx, extension, capture, options)
	}
	if options.DescribeImages && !format.DescribeImages {
		return rejection("image interpretation is unavailable for this format")
	}
	if options.ExternalOCR && options.DescribeImages {
		return rejection("choose external OCR or figure interpretation separately")
	}
	images, err := sourceImages(ctx, extension, capture.Original, options)
	if err != nil {
		return err
	}
	result, err := h.vision(ctx, images, options.ExternalOCR)
	if err != nil {
		return err
	}
	capture.Record.Warning = strings.ReplaceAll(capture.Record.Warning, "No external upload was performed.", "")
	if options.ExternalOCR {
		for _, section := range result.Sections {
			number := section.Number
			section.Number = 1
			one := result
			one.Sections = []documentSection{section}
			text, _, _, err := renderDocumentResult(one)
			if err != nil {
				return err
			}
			text = strings.TrimPrefix(text, "## Page 1\n\n")
			prefix := fmt.Sprintf("## Page %d\n\n", number)
			start := strings.Index(capture.Text, prefix)
			if start < 0 {
				return errors.New("native physical page missing before hosted acceptance")
			}
			end := len(capture.Text)
			if next := strings.Index(capture.Text[start+len(prefix):], "## Page "); next >= 0 {
				end = start + len(prefix) + next
			}
			capture.Text = capture.Text[:start] + prefix + text + capture.Text[end:]
		}
		capture.Record.Extractor += "; " + result.Extractor
		if capture.Record.Extraction != "partial" {
			capture.Record.Extraction = "extracted"
		}
		capture.Record.Warning += " Hosted OCR was explicitly selected and is unverified; compare tables, equations and quotations with the original."
	} else {
		capture.Text += "\n\n## AI image descriptions (unverified)\n\n"
		for _, section := range result.Sections {
			capture.Text += fmt.Sprintf("### Figure %d\n\n", section.Number)
			for _, block := range section.Blocks {
				if block.Kind != "text" {
					return errors.New("hosted description returned unexpected non-text content")
				}
				text, err := documentMarkdown(block)
				if err != nil {
					return err
				}
				capture.Text += text + "\n\n"
			}
		}
		capture.Record.Extractor += "; " + result.Extractor
		capture.Record.Warning += " Hosted figure descriptions were explicitly selected and may contain errors."
	}
	if result.Partial {
		capture.Record.Extraction = "partial"
		capture.Record.Warning += " The hosted result reports uncertain or unreadable content."
	}
	if len(capture.Text) > workspace.MaxSourceText() {
		return errors.New("hosted extraction exceeds source text budget")
	}
	return h.authorize(ctx)
}
func (h *Hosted) media(ctx context.Context, extension string, capture *workspace.SourceCapture, options Options) error {
	if h.config.TranscriptionModel == "" {
		return errors.New("configure a hosted transcription model before uploading media")
	}
	dir, err := scratchDirectory(ctx, "forgetmenote-audio-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "source"+extension)
	if err := os.WriteFile(filename, capture.Original, 0600); err != nil {
		return err
	}
	duration, err := mediaDuration(ctx, dir, filename)
	if err != nil {
		return err
	}
	result := documentResult{Extractor: "OpenAI hosted " + h.config.TranscriptionModel + " transcription v1; FFmpeg mono 16kHz", Unit: "segment"}
	for start := 0; float64(start) < duration; start += 30 {
		if options.pageStart != nil {
			if err := options.pageStart(start/30 + 1); err != nil {
				return err
			}
		}
		chunk := filepath.Join(dir, "segment.wav")
		if _, err := sourceCommand(ctx, dir, 1024, "ffmpeg", "-nostdin", "-v", "error", "-y", "-protocol_whitelist", "file,pipe", "-format_whitelist", "wav,mp3,mov,aac,ogg,flac,avi", "-i", filename, "-ss", strconv.Itoa(start), "-t", "30", "-vn", "-ac", "1", "-ar", "16000", "-fs", "1000000", "-f", "wav", chunk); err != nil {
			return err
		}
		info, err := os.Stat(chunk)
		if err != nil || info.Size() > 1000000 {
			return errors.New("media segment exceeds scratch budget")
		}
		wav, err := os.ReadFile(chunk)
		if err != nil {
			return err
		}
		text, err := h.transcribe(ctx, wav)
		os.Remove(chunk)
		if err != nil {
			return err
		}
		result.Sections = append(result.Sections, documentSection{Number: start/30 + 1, Title: fmt.Sprintf("%d:%02d–%d:%02d", start/60, start%60, min(start+30, int(math.Ceil(duration)))/60, min(start+30, int(math.Ceil(duration)))%60), Blocks: []documentBlock{{Kind: "text", Text: text}}})
	}
	text, extractor, state, err := renderDocumentResult(result)
	if err != nil {
		return err
	}
	capture.Text = text
	capture.Record.Extractor = extractor
	capture.Record.Extraction = state
	capture.Record.Warning = "Hosted transcription was explicitly selected. Segment references cover 30-second audio chunks, not word timestamps. Speech may be inaccurate; video images are not analyzed."
	return h.authorize(ctx)
}

func mediaDuration(ctx context.Context, dir, filename string) (float64, error) {
	raw, err := sourceCommand(ctx, dir, 16<<10, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", "wav,mp3,mov,aac,ogg,flac,avi", "-show_entries", "format=duration", "-of", "json", filename)
	if errors.As(err, new(toolFailure)) {
		return 0, rejection("media could not be read")
	}
	if err != nil {
		return 0, err
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return 0, errors.New("invalid media duration")
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 300 {
		return 0, rejection("media must have a known duration of at most five minutes")
	}
	return duration, nil
}
func mediaOriginal(ctx context.Context, extension string, raw []byte) (string, error) {
	dir, err := scratchDirectory(ctx, "forgetmenote-media-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "source"+extension)
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		return "", err
	}
	if _, err := mediaDuration(ctx, dir, filename); err != nil {
		return "", err
	}
	return "Original retained without transcription. Select hosted transcription explicitly if enabled in Administration. Video images are not analyzed.", nil
}
