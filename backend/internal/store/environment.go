package store

import (
	"context"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"

	"github.com/google/uuid"
)

// normalizeEnvName trims and lowercases name and checks it fits the column.
// action ("create", "update") goes into the error message.
func normalizeEnvName(action, name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)

	if name == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" an environment with an empty name")
	}
	// The column is VARCHAR(100), which counts characters, not bytes.
	if utf8.RuneCountInString(name) > 100 {
		return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" an environment with a name over 100 characters")
	}
	return name, nil
}

func (s *Store) CreateEnv(ctx context.Context, name string) (database.Environment, error) {
	name, err := normalizeEnvName("create", name)
	if err != nil {
		return database.Environment{}, err
	}
	env, err := s.Queries.CreateEnv(ctx, name)
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err)
	}
	return env, nil
}

func (s *Store) GetEnv(ctx context.Context, id uuid.UUID) (database.Environment, error) {
	env, err := s.Queries.GetEnv(ctx, id)
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err)
	}
	return env, nil
}

// UpdateEnv renames the environment with the given id, applying the same
// name rules as CreateEnv.
func (s *Store) UpdateEnv(ctx context.Context, id uuid.UUID, name string) (database.Environment, error) {
	name, err := normalizeEnvName("update", name)
	if err != nil {
		return database.Environment{}, err
	}
	env, err := s.Queries.UpdateEnv(ctx, database.UpdateEnvParams{ID: id, Name: name})
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err)
	}
	return env, nil
}

func (s *Store) ListEnvs(ctx context.Context) ([]database.Environment, error) {
	envs, err := s.Queries.ListEnvs(ctx)
	if err != nil {
		return nil, ffserrors.FromDBErr(err)
	}
	return envs, nil
}

// DeleteEnv deletes the environment with the given id and returns it as it
// was.
func (s *Store) DeleteEnv(ctx context.Context, id uuid.UUID) (database.Environment, error) {
	env, err := s.Queries.DeleteEnv(ctx, id)
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err)
	}
	return env, nil
}
