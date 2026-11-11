package utils

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

// GenerateSalt returns length random bytes in hex, so twice as many characters.
func GenerateSalt(length int) string {
	bytes := make([]byte, length)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// EncodePageCursor turns cursor into an opaque page token: its JSON in
// unpadded URL-safe base64.
func EncodePageCursor[T any](cursor *T) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	return token, nil
}

// DecodePageCursor gives the cursor a token from EncodePageCursor holds.
func DecodePageCursor[T any](token string) (*T, error) {
	var cursor T
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return &cursor, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&cursor); err != nil {
		return &cursor, err
	}
	return &cursor, nil
}
