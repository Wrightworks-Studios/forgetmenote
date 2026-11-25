package gitobj

import "strings"

// A Trailer is one "Key: value" line of a commit message's final trailer block.
type Trailer struct{ Key, Value string }

// These rules follow Git's trailer.c, sequencer.c and strbuf.c (Git 2.54) with
// the default configuration: comment character '#', separator ':', and new
// trailers added at the end unless the last trailer is the same.

const commentPrefix = "#"

var generatedPrefixes = []string{"Signed-off-by: ", "(cherry picked from commit "}

// Git's isspace: no vertical tab or form feed.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

// Trailers returns the trailers of message's final trailer block, as
// "git interpret-trailers --parse --no-divider" or the %(trailers:only,unfold)
// log format reports them: comment lines skipped, continuation lines unfolded.
// A commit message has no patch, so a "---" line ends nothing.
func Trailers(message string) []Trailer {
	end := endOfLogMessage(message)
	var trailers []Trailer
	for _, item := range parseBlock(message[trailerBlockStart(message, end):end], true) {
		if item.trailer {
			trailers = append(trailers, Trailer{Key: item.token, Value: item.value})
		}
	}
	return trailers
}

// CleanMessage returns the message "git commit -m subject -m body --trailer
// 'Key: Value'..." stores with the default whitespace cleanup. An empty body is
// the same as leaving out its -m.
func CleanMessage(subject, body string, trailers []Trailer) string {
	message := subject
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	message += "\n" + body
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	message = stripSpace(message)
	if len(trailers) > 0 {
		message = stripSpace(addTrailers(message, trailers))
	}
	return message
}

// stripSpace is strbuf_stripspace without comment removal: trailing spaces
// cut from each line, runs of empty lines collapsed, empty lines removed from
// both ends, and every line ending in a newline.
func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	empties := 0
	for len(s) > 0 {
		line := s
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			line, s = s[:nl+1], s[nl+1:]
		} else {
			s = ""
		}
		n := len(line)
		for n > 0 && isSpace(line[n-1]) {
			n--
		}
		if n == 0 {
			empties++
			continue
		}
		if empties > 0 && b.Len() > 0 {
			b.WriteByte('\n')
		}
		empties = 0
		b.WriteString(line[:n])
		b.WriteByte('\n')
	}
	return b.String()
}

// lines returns the start offset of each line of s[:end].
func lineStarts(s string, end int) []int {
	var starts []int
	for i := 0; i < end; {
		starts = append(starts, i)
		nl := strings.IndexByte(s[i:end], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	return starts
}

func lineAt(s string, i int) string {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return s[i : i+nl+1]
	}
	return s[i:]
}

func isBlankLine(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] == '\n' {
			return true
		}
		if !isSpace(line[i]) {
			return false
		}
	}
	return true
}

// endOfLogMessage drops the trailing comment and blank lines Git ignores, and
// anything from a scissors line on.
func endOfLogMessage(s string) int {
	cutoff := len(s)
	const scissors = "\n" + commentPrefix + " ------------------------ >8 ------------------------"
	if strings.HasPrefix(s, scissors[1:]) {
		cutoff = 0
	} else if i := strings.Index(s, scissors); i >= 0 && i+1 < cutoff {
		cutoff = i + 1
	}
	// Git keeps the start of the trailing run in boc with 0 meaning unset, so a
	// run that begins at the very first byte starts at its second line.
	boc, inConflicts := 0, false
	for bol := 0; bol < cutoff; {
		next := len(s)
		if nl := strings.IndexByte(s[bol:], '\n'); nl >= 0 {
			next = bol + nl + 1
		}
		switch {
		case strings.HasPrefix(s[bol:cutoff], commentPrefix) || s[bol] == '\n':
			if boc == 0 {
				boc = bol
			}
		case strings.HasPrefix(s[bol:], "Conflicts:\n"):
			inConflicts = true
			if boc == 0 {
				boc = bol
			}
		case inConflicts && s[bol] == '\t':
		case boc != 0:
			boc, inConflicts = 0, false
		}
		bol = next
	}
	if boc != 0 {
		return boc
	}
	return cutoff
}

// findSeparator returns the offset of the first separator in line after a
// token of letters, digits and hyphens, perhaps followed by spaces, or -1.
func findSeparator(line, separators string) int {
	whitespace := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if strings.IndexByte(separators, c) >= 0 {
			return i
		}
		if !whitespace && (isAlnum(c) || c == '-') {
			continue
		}
		if i > 0 && (c == ' ' || c == '\t') {
			whitespace = true
			continue
		}
		break
	}
	return -1
}

// trailerBlockStart finds the final paragraph of s[:end] that is all
// trailers, or holds a Git-generated trailer and at least 25% trailers. It
// returns end when there is none. The first paragraph is the title and never
// trailers.
func trailerBlockStart(s string, end int) int {
	starts := lineStarts(s, end)
	title := len(starts)
	for i, start := range starts {
		line := lineAt(s, start)
		if strings.HasPrefix(s[start:end], commentPrefix) {
			continue
		}
		if isBlankLine(line) {
			title = i
			break
		}
	}
	endOfTitle := end
	if title < len(starts) {
		endOfTitle = starts[title]
	}
	onlySpaces, recognized := true, false
	trailerLines, nonTrailerLines, continuations := 0, 0, 0
	for i := len(starts) - 1; i >= 0 && starts[i] >= endOfTitle; i-- {
		start := starts[i]
		line := lineAt(s, start)
		if strings.HasPrefix(s[start:end], commentPrefix) {
			nonTrailerLines += continuations
			continuations = 0
			continue
		}
		if isBlankLine(line) {
			if onlySpaces {
				continue
			}
			nonTrailerLines += continuations
			next := start + len(line)
			if recognized && trailerLines*3 >= nonTrailerLines {
				return next
			}
			if trailerLines > 0 && nonTrailerLines == 0 {
				return next
			}
			return end
		}
		onlySpaces = false
		generated := false
		for _, prefix := range generatedPrefixes {
			if strings.HasPrefix(s[start:], prefix) {
				generated = true
			}
		}
		if generated {
			trailerLines++
			continuations = 0
			recognized = true
			continue
		}
		if separator := findSeparator(line, ":"); separator >= 1 && !isSpace(line[0]) {
			trailerLines++
			continuations = 0
		} else if isSpace(line[0]) {
			continuations++
		} else {
			nonTrailerLines += 1 + continuations
			continuations = 0
		}
	}
	return end
}

type blockItem struct {
	trailer      bool
	token, value string
}

// parseBlock splits a trailer block into its trailers, each with its
// continuation lines, and its other lines. Comment lines are dropped, as Git
// drops them when it rewrites the block.
func parseBlock(block string, unfold bool) []blockItem {
	var pieces []string
	last := -1
	for _, start := range lineStarts(block, len(block)) {
		line := lineAt(block, start)
		if last >= 0 && isSpace(line[0]) {
			pieces[last] += line
			continue
		}
		pieces = append(pieces, line)
		last = -1
		if findSeparator(line, ":") >= 1 {
			last = len(pieces) - 1
		}
	}
	var items []blockItem
	for _, piece := range pieces {
		if strings.HasPrefix(piece, commentPrefix) {
			continue
		}
		if separator := findSeparator(piece, ":"); separator >= 1 {
			value := trimSpace(piece[separator+1:])
			if unfold {
				value = unfoldValue(value)
			}
			items = append(items, blockItem{trailer: true, token: trimSpace(piece[:separator]), value: value})
		} else {
			items = append(items, blockItem{value: strings.TrimSuffix(piece, "\n")})
		}
	}
	return items
}

func unfoldValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); {
		c := v[i]
		i++
		if c == '\n' {
			for i < len(v) && isSpace(v[i]) {
				i++
			}
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	return trimSpace(b.String())
}

func tokenLen(token string) int {
	n := len(token)
	for n > 0 && !isAlnum(token[n-1]) {
		n--
	}
	return n
}

func sameToken(a blockItem, token string) bool {
	if !a.trailer {
		return false
	}
	n := min(tokenLen(a.token), tokenLen(token))
	return equalFoldASCII(a.token[:n], token[:n])
}

// equalFoldASCII is strcasecmp's equality: only ASCII letters fold.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// addTrailers is "git interpret-trailers --in-place --no-divider --trailer
// 'Key: Value'..." on a cleaned message.
func addTrailers(message string, trailers []Trailer) string {
	end := endOfLogMessage(message)
	start := trailerBlockStart(message, end)
	items := parseBlock(message[start:end], false)
	for _, t := range trailers {
		arg := t.Key + ": " + t.Value
		separator := findSeparator(arg, "=:")
		if separator == 0 {
			continue // Git refuses an empty token and adds nothing.
		}
		item := blockItem{trailer: true}
		if separator > 0 {
			item.token, item.value = trimSpace(arg[:separator]), trimSpace(arg[separator+1:])
		} else {
			item.token = trimSpace(arg)
		}
		exists := false
		for i := len(items) - 1; i >= 0; i-- {
			if sameToken(items[i], item.token) {
				exists = true
				break
			}
		}
		if exists {
			last := items[len(items)-1]
			if sameToken(last, item.token) && equalFoldASCII(last.value, item.value) {
				continue
			}
		}
		items = append(items, item)
	}
	var b strings.Builder
	b.WriteString(message[:start])
	if lines := lineStarts(message, start); len(lines) == 0 || !isBlankLine(lineAt(message, lines[len(lines)-1])) {
		b.WriteByte('\n')
	}
	for _, item := range items {
		if item.trailer {
			// Tokens are trimmed, so the last byte is Git's last non-space one.
			b.WriteString(item.token)
			if n := len(item.token); n > 0 && item.token[n-1] != ':' {
				b.WriteString(": ")
			}
			b.WriteString(item.value)
		} else {
			b.WriteString(item.value)
		}
		b.WriteByte('\n')
	}
	b.WriteString(message[end:])
	return b.String()
}
