package store

import (
	"context"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
)

func (s *Store) CreateEnv(ctx context.Context, name string) (database.Environment, error) {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)

	if name == "" {
		return database.Environment{}, connect.NewError(connect.CodeInvalidArgument, "can't create an environment with an empty name")
	}
	// The column is VARCHAR(100), which counts characters, not bytes.
	if utf8.RuneCountInString(name) > 100 {
		return database.Environment{}, connect.NewError(connect.CodeInvalidArgument, "can't create an environment with a name over 100 characters")
	}
	env, err := s.Queries.CreateEnv(ctx, name)
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err)
	}
	return env, nil
}
