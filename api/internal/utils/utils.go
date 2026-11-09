package utils

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateSalt returns length random bytes in hex, so twice as many characters.
func GenerateSalt(length int) string {
	bytes := make([]byte, length)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
