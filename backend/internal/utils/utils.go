package utils

import (
	"crypto/rand"
	"encoding/hex"

	connect "connectrpc.com/connect/v2"
)

func CodeToText(code connect.Code) string {
	var text string
	switch code {
	case connect.CodeAlreadyExists:
		text = "can't duplicate"
	case connect.CodeNotFound:
		text = "not found"
	default:
		text = "something went wrong"
	}
	return text
}

func GenerateSalt(length int) string {
	bytes := make([]byte, length)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
