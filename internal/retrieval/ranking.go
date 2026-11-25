// Package retrieval contains ranking math and evidence budgets shared by notes and source.
package retrieval

import (
	"math"
	"strings"
	"unicode"
)

func Tokens(text string) []string {
	result := []string{}
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			result = append(result, word.String())
			word.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (unicode.IsMark(r) && word.Len() > 0) {
			word.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return result
}

// SourceTokens retains identifiers as well as snake/camel case components.
func SourceTokens(text string) []string {
	result := Tokens(text)
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if strings.Contains(word, "_") {
			result = append(result, strings.ToLower(word))
		}
		rr := []rune(word)
		start := 0
		for i := 1; i < len(rr); i++ {
			if unicode.IsUpper(rr[i]) && (unicode.IsLower(rr[i-1]) || (i+1 < len(rr) && unicode.IsLower(rr[i+1]) && unicode.IsUpper(rr[i-1]))) {
				if start < i {
					result = append(result, strings.ToLower(string(rr[start:i])))
				}
				start = i
			}
		}
		if start > 0 {
			result = append(result, strings.ToLower(string(rr[start:])))
		}
	}
	return result
}
func IDF(documents, frequency int) float64 {
	return math.Log(1 + (float64(documents-frequency)+.5)/(float64(frequency)+.5))
}
func TermScore(tf, length int, average, idf, weight float64) float64 {
	if tf == 0 || average <= 0 {
		return 0
	}
	norm := 1 - .75 + .75*float64(length)/average
	return weight * idf * float64(tf) * 2.2 / (float64(tf) + 1.2*norm)
}
func ReciprocalRank(rank int) float64 { return 1 / float64(61+rank) }
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var n float64
	for i := range a {
		n += float64(a[i]) * float64(b[i])
	}
	return n
}

func Framing(term string) bool {
	switch term {
	case "a", "an", "the", "about", "of", "for", "to", "with", "on", "in", "from", "by", "at", "and", "or", "as", "that", "this", "these", "those", "it", "its", "i", "we", "you", "your", "me", "my", "us", "our", "is", "are", "was", "were", "be", "been", "being", "do", "does", "did", "can", "could", "would", "should", "will", "what", "which", "who", "where", "when", "why", "how", "please", "tell", "show", "explain", "describe", "summarize", "compare":
		return true
	}
	return false
}
