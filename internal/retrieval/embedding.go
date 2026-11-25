package retrieval

import "strings"

// EmbeddingModelKey versions preprocessing separately from the provider's model
// name so previously stored, unprefixed vectors cannot enter a retrieval index.
func EmbeddingModelKey(model string) string {
	name, _, _ := strings.Cut(model, ":")
	if name == "embeddinggemma" {
		return model + "\nembeddinggemma-retrieval-v1"
	}
	return model
}

// EmbeddingInput follows Google's query/document retrieval prompts. Unknown
// models retain their operator-selected input contract without guessed prefixes.
func EmbeddingInput(model, text string, document bool) string {
	if EmbeddingModelKey(model) == model {
		return text
	}
	if document {
		return "title: none | text: " + text
	}
	return "task: search result | query: " + text
}
