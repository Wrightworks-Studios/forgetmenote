package chat

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

// OpenAI documents a 512 MiB total image request payload.
const openAIImagePayloadLimit = 512 << 20

// InlineImage accepts image bytes without imposing pixel or dimension limits.
// Fetching owns network authorization separately from the model transport.
func InlineImage(data []byte) (string, error) {
	if len(data) == 0 || len(data) > openAIImagePayloadLimit {
		return "", providerContractError("image exceeds OpenAI 512 MiB payload limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 {
		return "", providerContractError("unsupported image")
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
func validateImages(m Message, c Connection) error {
	return validateImagesLimit(m, c, 4)
}
func validateImagesLimit(m Message, c Connection, maxImages int) error {
	if len(m.Images) == 0 {
		return nil
	}
	if !c.Capabilities.Images || c.Format == "messages" || m.Role != "user" || len(m.Images) > maxImages {
		return providerContractError("images require a configured vision connection and a user message")
	}
	payloadBytes := 0
	for _, value := range m.Images {
		payloadBytes += len(value)
		if payloadBytes > openAIImagePayloadLimit {
			return providerContractError("images exceed OpenAI 512 MiB request payload limit")
		}
		head, encoded, ok := strings.Cut(value, ",")
		if !ok || !strings.HasPrefix(head, "data:image/") || !strings.HasSuffix(head, ";base64") || len(encoded) > 1<<30 {
			return providerContractError("images must be inline data within OpenAI image URL limits")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return providerContractError("invalid inline image")
		}
		canonical, err := InlineImage(data)
		if err != nil {
			return err
		}
		if canonical != value {
			return providerContractError("image MIME does not match bytes")
		}
	}
	return nil
}
