package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
)

// normalizeFlagKey trims and lowercases key and checks it only holds ASCII
// letters, digits and "-". action ("create", "update") goes into the error
// message.
func normalizeFlagKey(action, key string) (string, error) {
	key = strings.TrimSpace(key)

	if key == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a flag with an empty key")
	}
	// Checked before lowercasing: a few non-ASCII characters, like the
	// Kelvin sign, lowercase to ASCII letters and would slip through.
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		case unicode.IsSpace(r):
			return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a flag with whitespace in its key")
		default:
			// The code point tells apart look-alikes such as the Kelvin sign
			// and "K".
			return "", connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("can't %s a flag with %q (%U) in its key: only letters, digits and - are allowed", action, r, r))
		}
	}
	// Only ASCII is left, so bytes and characters are the same.
	if len(key) > 100 {
		return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a flag with a key over 100 characters")
	}
	return strings.ToLower(key), nil
}

func (s *Store) CreateFlag(ctx context.Context, key string) (database.Flag, error) {
	key, err := normalizeFlagKey("create", key)
	if err != nil {
		return database.Flag{}, err
	}
	flag, err := s.Queries.CreateFlag(ctx, key)
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err)
	}
	return flag, nil
}
