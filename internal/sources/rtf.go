//go:build !fmn_omit_outlook

package sources

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strconv"
	"strings"
	"unicode/utf16"

	"forgetmenote/internal/workspace"
	"golang.org/x/text/encoding/charmap"
)

// MS-OXRTFCP uses a 4096-byte circular dictionary and a CRC with initial zero
// and no final XOR (unlike the usual IEEE checksum convention).
func decompressRTF(ctx context.Context, raw []byte) ([]byte, error) {
	invalid := errors.New("invalid or corrupt compressed Outlook RTF")
	if len(raw) < 16 {
		return nil, invalid
	}
	size := int(binary.LittleEndian.Uint32(raw[:4]))
	length := int(binary.LittleEndian.Uint32(raw[4:8]))
	if size < 12 || size != len(raw)-4 || length > workspace.MaxSourceText()*2 {
		return nil, invalid
	}
	payload := raw[16:]
	crc := binary.LittleEndian.Uint32(raw[12:16])
	if string(raw[8:12]) == "MELA" {
		if crc != 0 || len(payload) != length {
			return nil, invalid
		}
		return payload, nil
	}
	if string(raw[8:12]) != "LZFu" {
		return nil, invalid
	}
	var actual uint32
	table := crc32.MakeTable(crc32.IEEE)
	for _, b := range payload {
		actual = table[byte(actual)^b] ^ (actual >> 8)
	}
	if actual != crc {
		return nil, invalid
	}
	const seed = "{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}{\\f0\\fnil \\froman \\fswiss \\fmodern \\fscript \\fdecor MS Sans SerifSymbolArialTimes New RomanCourier{\\colortbl\\red0\\green0\\blue0\r\n\\par \\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx"
	var dictionary [4096]byte
	for i := range dictionary {
		dictionary[i] = ' '
	}
	copy(dictionary[:], seed)
	write := len(seed)
	out := make([]byte, 0, length)
	appendByte := func(b byte) error {
		if len(out) >= length {
			return invalid
		}
		out = append(out, b)
		dictionary[write] = b
		write = (write + 1) % 4096
		return nil
	}
	for cursor := 0; cursor < len(payload); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		control := payload[cursor]
		cursor++
		for bit := 0; bit < 8; bit++ {
			if cursor >= len(payload) {
				return nil, invalid
			}
			if control&(1<<bit) == 0 {
				b := payload[cursor]
				cursor++
				if err := appendByte(b); err != nil {
					return nil, err
				}
				continue
			}
			if len(payload)-cursor < 2 {
				return nil, invalid
			}
			token := binary.BigEndian.Uint16(payload[cursor : cursor+2])
			cursor += 2
			offset := int(token >> 4)
			if offset == write {
				if len(out) != length {
					return nil, invalid
				}
				return out, nil
			}
			for j := 0; j < int(token&15)+2; j++ {
				if err := appendByte(dictionary[(offset+j)%4096]); err != nil {
					return nil, err
				}
			}
		}
	}
	return nil, invalid
}
func outlookRTF(ctx context.Context, compressed []byte) (string, error) {
	raw, err := decompressRTF(ctx, compressed)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(raw), "{\\rtf") {
		return "", errors.New("invalid Outlook RTF body")
	}
	type group struct {
		skip bool
		uc   int
	}
	state := group{uc: 1}
	var stack []group
	var out boundedText
	fallback := 0
	var high uint16
	put := func(r rune) error {
		if state.skip {
			return nil
		}
		_, err := out.Write([]byte(string(r)))
		return err
	}
	for i := 0; i < len(raw); {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c := raw[i]
		i++
		switch c {
		case '{':
			stack = append(stack, state)
			if len(stack) > 128 {
				return "", errors.New("Outlook RTF nesting exceeds budget")
			}
		case '}':
			if len(stack) == 0 {
				return "", errors.New("invalid Outlook RTF groups")
			}
			state = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case '\r', '\n':
		case '\\':
			if i >= len(raw) {
				return "", errors.New("truncated Outlook RTF control")
			}
			c = raw[i]
			i++
			if c == '\\' || c == '{' || c == '}' {
				if fallback > 0 {
					fallback--
				} else if err := put(rune(c)); err != nil {
					return "", err
				}
				continue
			}
			if c == '*' {
				state.skip = true
				continue
			}
			if c == '\'' {
				if len(raw)-i < 2 {
					return "", errors.New("truncated Outlook RTF escape")
				}
				value, err := strconv.ParseUint(string(raw[i:i+2]), 16, 8)
				if err != nil {
					return "", err
				}
				i += 2
				if fallback > 0 {
					fallback--
				} else if err := put(charmap.Windows1252.DecodeByte(byte(value))); err != nil {
					return "", err
				}
				continue
			}
			if c == '~' {
				if err := put(' '); err != nil {
					return "", err
				}
				continue
			}
			if c < 'a' || c > 'z' {
				continue
			}
			start := i - 1
			for i < len(raw) && raw[i] >= 'a' && raw[i] <= 'z' {
				i++
			}
			word := string(raw[start:i])
			start = i
			if i < len(raw) && raw[i] == '-' {
				i++
			}
			for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
				i++
			}
			number := 0
			if i > start {
				number, err = strconv.Atoi(string(raw[start:i]))
				if err != nil {
					return "", err
				}
			}
			if i < len(raw) && raw[i] == ' ' {
				i++
			}
			switch word {
			case "fonttbl", "colortbl", "stylesheet", "info", "pict", "object", "fldinst", "htmltag", "listtable", "listoverridetable":
				state.skip = true
			case "bin":
				if number < 0 || number > len(raw)-i {
					return "", errors.New("invalid Outlook RTF binary length")
				}
				i += number
			case "uc":
				if number < 0 || number > 16 {
					return "", errors.New("invalid Outlook RTF Unicode fallback")
				}
				state.uc = number
			case "u":
				unit := uint16(number)
				r := rune(unit)
				if unit >= 0xd800 && unit <= 0xdbff {
					high = unit
				} else {
					if high != 0 {
						r = utf16.DecodeRune(rune(high), r)
						high = 0
					}
					if err := put(r); err != nil {
						return "", err
					}
				}
				fallback = state.uc
			case "par", "line", "row":
				if err := put('\n'); err != nil {
					return "", err
				}
			case "tab", "cell":
				if err := put('\t'); err != nil {
					return "", err
				}
			case "emdash":
				if err := put('—'); err != nil {
					return "", err
				}
			case "endash":
				if err := put('–'); err != nil {
					return "", err
				}
			}
		default:
			if fallback > 0 {
				fallback--
			} else if err := put(charmap.Windows1252.DecodeByte(c)); err != nil {
				return "", err
			}
		}
	}
	if len(stack) != 0 {
		return "", errors.New("unclosed Outlook RTF group")
	}
	return strings.TrimSpace(out.String()), nil
}
