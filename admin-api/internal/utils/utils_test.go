package utils

import (
	"encoding/base64"
	"strings"
	"testing"
)

type testCursor struct {
	Key string `json:"key"`
	N   int    `json:"n"`
}

func TestPageCursorRoundTrip(t *testing.T) {
	tests := map[string]testCursor{
		"zero cursor":          {},
		"key":                  {Key: "new-checkout"},
		"key and number":       {Key: "a-b", N: 42},
		"characters to escape": {Key: `"quoted" \ and ünïcode`, N: -1},
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			token, err := EncodePageCursor(&want)
			if err != nil {
				t.Fatalf("EncodePageCursor(%+v): %v", want, err)
			}
			got, err := DecodePageCursor[testCursor](token)
			if err != nil {
				t.Fatalf("DecodePageCursor(%q): %v", token, err)
			}
			if got == nil || *got != want {
				t.Fatalf("DecodePageCursor(EncodePageCursor(%+v)) = %+v", want, got)
			}
		})
	}
}

// Tokens go in URLs and query strings, so they hold only URL-safe base64
// characters, without padding.
func TestEncodePageCursorIsURLSafe(t *testing.T) {
	// This cursor's JSON, {"key":"??>","n":0}, is 19 bytes, which standard
	// base64 pads, and holds "?" bytes that standard base64 turns into "/".
	token, err := EncodePageCursor(&testCursor{Key: "??>"})
	if err != nil {
		t.Fatalf("EncodePageCursor(): %v", err)
	}
	if strings.ContainsAny(token, "+/=") {
		t.Fatalf("EncodePageCursor() = %q, want no +, / or =", token)
	}
	if _, err := base64.RawURLEncoding.DecodeString(token); err != nil {
		t.Fatalf("EncodePageCursor() = %q, not unpadded URL-safe base64: %v", token, err)
	}
}

func TestEncodePageCursorReportsErrors(t *testing.T) {
	f := func() {}
	if _, err := EncodePageCursor(&f); err == nil {
		t.Fatal("EncodePageCursor(func) = nil error, want the JSON error")
	}
}

func TestDecodePageCursorRejectsBadTokens(t *testing.T) {
	tests := map[string]string{
		"empty token":                    "",
		"not base64":                     "!!!",
		"padded base64":                  base64.URLEncoding.EncodeToString([]byte(`{"key":"a"}`)),
		"base64 of non-JSON":             base64.RawURLEncoding.EncodeToString([]byte("not json")),
		"a JSON field of the wrong type": base64.RawURLEncoding.EncodeToString([]byte(`{"key":1}`)),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := DecodePageCursor[testCursor](token)
			if err == nil {
				t.Fatalf("DecodePageCursor(%q) = %+v, want an error", token, got)
			}
			if got != nil && *got != (testCursor{}) {
				t.Fatalf("DecodePageCursor(%q) = %+v on error, want nil or the zero cursor", token, *got)
			}
		})
	}
}
