package store

import (
	"context"
	"strings"
	"unicode/utf8"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
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

// CreateEnv adds an environment, with its name normalized by normalizeEnvName,
// and a disabled setting for every existing flag, in one transaction.
func (s *Store) CreateEnv(ctx context.Context, name string) (database.Environment, error) {
	name, err := normalizeEnvName("create", name)
	if err != nil {
		return database.Environment{}, err
	}
	var env database.Environment
	err = s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		e, err := q.CreateEnv(ctx, name)
		if err != nil {
			return err
		}
		env = e
		return q.CreateSettingsForEnv(ctx, e.ID)
	})
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err, "can't create the environment")
	}
	return env, nil
}

// GetEnv gets the environment with the given id.
func (s *Store) GetEnv(ctx context.Context, id uuid.UUID) (database.Environment, error) {
	env, err := s.Queries.GetEnv(ctx, id)
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err, "can't get the environment")
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
		return database.Environment{}, ffserrors.FromDBErr(err, "can't update the environment")
	}
	return env, nil
}

// ListEnvs gets every environment, oldest first.
func (s *Store) ListEnvs(ctx context.Context) ([]database.Environment, error) {
	envs, err := s.Queries.ListEnvs(ctx)
	if err != nil {
		return nil, ffserrors.FromDBErr(err, "can't list environments")
	}
	return envs, nil
}

// DeleteEnv deletes the environment with the given id, along with its settings
// and their rules, logs the deletion at the version after the one it was at, and
// returns it as it was.
func (s *Store) DeleteEnv(ctx context.Context, id uuid.UUID) (database.Environment, error) {
	var env database.Environment
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		e, err := q.DeleteEnv(ctx, id)
		if err != nil {
			return err
		}
		env = e
		return q.CreateChangeLogsForEnvDelete(ctx, database.CreateChangeLogsForEnvDeleteParams{
			EnvironmentID: e.ID,
			Version:       e.Version + 1,
		})
	})
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err, "can't delete the environment")
	}
	return env, nil
}
