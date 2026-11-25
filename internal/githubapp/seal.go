package githubapp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
)

// SealToken encrypts an installation token for state that every gateway
// shares, under a key derived from the App's private key, which only the
// gateways hold. A database copy alone therefore reveals no token.
func (g *Client) SealToken(token string) (string, error) {
	aead, err := g.sealer()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(token), nil)), nil
}

// OpenToken returns a token SealToken sealed under the same App key.
func (g *Client) OpenToken(sealed string) (string, error) {
	aead, err := g.sealer()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("invalid sealed token")
	}
	token, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("invalid sealed token")
	}
	return string(token), nil
}

func (g *Client) sealer() (cipher.AEAD, error) {
	if g == nil || g.key == nil {
		return nil, ErrDenied
	}
	key, err := hkdf.Key(sha256.New, x509.MarshalPKCS1PrivateKey(g.key), nil, "forgetmenote installation token seal v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
