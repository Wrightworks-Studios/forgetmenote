package sources

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf16"

	"forgetmenote/internal/workspace"
	"github.com/richardlehane/mscfb"
	"golang.org/x/text/encoding/charmap"
)

// CFB can advertise enormous allocations or cyclic stream chains. The header
// counts must fit the actual file, and reads have a finite work budget.
type compoundReader struct {
	ctx   context.Context
	raw   *bytes.Reader
	reads int
}

func (r *compoundReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	r.reads++
	if r.reads > 200000 {
		return 0, errors.New("Outlook container exceeds read-work budget")
	}
	return r.raw.ReadAt(p, offset)
}
func outlookText(ctx context.Context, raw []byte) (string, error) {
	if len(raw) < 512 || !bytes.Equal(raw[:8], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
		return "", errors.New("invalid Outlook compound file")
	}
	shift := binary.LittleEndian.Uint16(raw[30:32])
	if shift != 9 && shift != 12 {
		return "", errors.New("invalid Outlook sector size")
	}
	sectors := uint32(len(raw) / (1 << shift))
	for _, offset := range []int{40, 44, 64, 72} {
		if binary.LittleEndian.Uint32(raw[offset:offset+4]) > sectors {
			return "", errors.New("Outlook sector count exceeds original file")
		}
	}
	if err := validateCompoundDirectory(ctx, raw, int(shift)); err != nil {
		return "", err
	}
	reader, err := mscfb.New(&compoundReader{ctx: ctx, raw: bytes.NewReader(raw)})
	if err != nil {
		return "", errors.New("invalid or corrupt Outlook compound file")
	}
	if len(reader.File) > 10000 {
		return "", errors.New("Outlook container exceeds 10000 streams")
	}
	properties := map[string][]byte{}
	var attachments []string
	total := 0
	for file, err := reader.Next(); err == nil; file, err = reader.Next() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if file.FileInfo().IsDir() {
			continue
		}
		name := strings.ToUpper(file.Name)
		attached := len(file.Path) == 1 && strings.HasPrefix(file.Path[0], "__attach_version1.0_")
		selected := len(file.Path) == 0 && strings.HasPrefix(name, "__SUBSTG1.0_") && (strings.HasPrefix(name[12:], "0037") || strings.HasPrefix(name[12:], "1000") || strings.HasPrefix(name[12:], "1013") || strings.HasPrefix(name[12:], "1009") || strings.HasPrefix(name[12:], "0C1A") || strings.HasPrefix(name[12:], "5D01") || strings.HasPrefix(name[12:], "0E04") || strings.HasPrefix(name[12:], "0E03"))
		if !selected && !(attached && (name == "__SUBSTG1.0_3707001F" || name == "__SUBSTG1.0_3704001F" || name == "__SUBSTG1.0_3707001E" || name == "__SUBSTG1.0_3704001E")) {
			continue
		}
		if file.Size < 0 || file.Size > int64(workspace.MaxSourceText()*2) {
			return "", errors.New("Outlook property exceeds text budget")
		}
		data, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, file.Size+1))
		if err != nil || int64(len(data)) != file.Size {
			return "", errors.New("corrupt Outlook property stream")
		}
		total += len(data)
		if total > workspace.MaxSourceText()*4 {
			return "", errors.New("Outlook decoded properties exceed text budget")
		}
		if attached {
			value, err := outlookString(data, strings.HasSuffix(name, "001F"))
			if err != nil {
				return "", err
			}
			attachments = append(attachments, value)
		} else {
			properties[strings.TrimPrefix(name, "__SUBSTG1.0_")] = data
		}
	}
	get := func(id string) (string, error) {
		if data, ok := properties[id+"001F"]; ok {
			return outlookString(data, true)
		}
		return outlookString(properties[id+"001E"], false)
	}
	var out boundedText
	for _, header := range []struct{ id, name string }{{"0037", "Subject"}, {"0C1A", "From"}, {"5D01", "Sender"}, {"0E04", "To"}, {"0E03", "Cc"}} {
		value, err := get(header.id)
		if err != nil {
			return "", err
		}
		if value != "" {
			if _, err = out.Write([]byte("**" + header.name + ":** " + workspace.EscapeSourceText(value) + "\n\n")); err != nil {
				return "", err
			}
		}
	}
	body, err := get("1000")
	if err != nil {
		return "", err
	}
	body = workspace.EscapeSourceText(body)
	if strings.TrimSpace(body) == "" && len(properties["10130102"]) > 0 {
		html := properties["10130102"]
		decoded, err := charmap.Windows1252.NewDecoder().Bytes(html)
		if err != nil {
			return "", err
		}
		// UTF-8 HTML remains UTF-8; ANSI message bodies use Windows-1252.
		if text, err := plainText(html); err == nil {
			decoded = []byte(text)
		}
		_, body, err = htmlTextContext(ctx, decoded, "text/html")
		if err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(body) == "" && len(properties["10090102"]) > 0 {
		body, err = outlookRTF(ctx, properties["10090102"])
		if err != nil {
			return "", err
		}
		body = workspace.EscapeSourceText(body)
	}
	if _, err = out.Write([]byte(body)); err != nil {
		return "", err
	}
	if len(attachments) > 0 {
		_, err = out.Write([]byte("\n\nAttachments (not opened): " + workspace.EscapeSourceText(strings.Join(attachments, ", "))))
		if err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

// The dependency materializes ancestor paths during construction. Bound the
// directory graph before that allocation phase, not after reader.File exists.
func validateCompoundDirectory(ctx context.Context, raw []byte, shift int) error {
	const end = uint32(0xfffffffe)
	const unused = uint32(0xffffffff)
	sectorSize := 1 << shift
	sector := func(id uint32) ([]byte, error) {
		offset := (uint64(id) + 1) * uint64(sectorSize)
		if offset+uint64(sectorSize) > uint64(len(raw)) {
			return nil, errors.New("invalid Outlook directory sector")
		}
		return raw[int(offset) : int(offset)+sectorSize], nil
	}
	count := int(binary.LittleEndian.Uint32(raw[44:48]))
	fat := make([]uint32, 0, count)
	add := func(data []byte) {
		for i := 0; i+4 <= len(data) && len(fat) < count; i += 4 {
			fat = append(fat, binary.LittleEndian.Uint32(data[i:]))
		}
	}
	add(raw[76:512])
	next := binary.LittleEndian.Uint32(raw[68:72])
	for i := 0; len(fat) < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i >= int(binary.LittleEndian.Uint32(raw[72:76])) || next == end || next == unused {
			return errors.New("invalid Outlook DIFAT chain")
		}
		data, err := sector(next)
		if err != nil {
			return err
		}
		add(data[:len(data)-4])
		next = binary.LittleEndian.Uint32(data[len(data)-4:])
	}
	chainNext := func(id uint32) (uint32, error) {
		index := int(id) / (sectorSize / 4)
		if index >= len(fat) {
			return 0, errors.New("invalid Outlook directory allocation")
		}
		data, err := sector(fat[index])
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(data[(int(id)%(sectorSize/4))*4:]), nil
	}
	type links struct{ left, right, child uint32 }
	var entries []links
	seenSectors := map[uint32]bool{}
	for next = binary.LittleEndian.Uint32(raw[48:52]); next != end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seenSectors[next] {
			return errors.New("cyclic Outlook directory sectors")
		}
		seenSectors[next] = true
		data, err := sector(next)
		if err != nil {
			return err
		}
		if len(entries)+len(data)/128 > 10000 {
			return errors.New("Outlook container exceeds 10000 directory entries")
		}
		for i := 0; i < len(data); i += 128 {
			entry := data[i : i+128]
			entries = append(entries, links{binary.LittleEndian.Uint32(entry[68:72]), binary.LittleEndian.Uint32(entry[72:76]), binary.LittleEndian.Uint32(entry[76:80])})
		}
		next, err = chainNext(next)
		if err != nil {
			return err
		}
	}
	type visit struct {
		id                 uint32
		nesting, recursion int
	}
	stack := []visit{{id: 0}}
	seen := make([]bool, len(entries))
	paths := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if int(v.id) >= len(entries) || seen[v.id] {
			return errors.New("invalid or cyclic Outlook directory graph")
		}
		if v.nesting > 64 || v.recursion > 256 {
			return errors.New("Outlook directory exceeds nesting budget")
		}
		seen[v.id] = true
		paths += v.nesting
		if paths > 100000 {
			return errors.New("Outlook directory exceeds ancestor path budget")
		}
		entry := entries[v.id]
		for _, id := range []uint32{entry.left, entry.right} {
			if id != unused {
				stack = append(stack, visit{id, v.nesting, v.recursion + 1})
			}
		}
		if entry.child != unused {
			nesting := v.nesting
			if v.id != 0 {
				nesting++
			}
			stack = append(stack, visit{entry.child, nesting, v.recursion + 1})
		}
	}
	return nil
}
func outlookString(raw []byte, unicode bool) (string, error) {
	if !unicode {
		decoded, err := charmap.Windows1252.NewDecoder().Bytes(raw)
		return strings.TrimRight(string(decoded), "\x00"), err
	}
	if len(raw)%2 != 0 {
		return "", errors.New("invalid Outlook UTF-16 property")
	}
	chars := make([]uint16, len(raw)/2)
	for i := range chars {
		chars[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return strings.TrimRight(string(utf16.Decode(chars)), "\x00"), nil
}
