package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/ffserrors"
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

// EnvsCursor marks where a page of ListEnvs ended: the next page starts at
// the first environment created after the one created at CreatedAt with ID.
// The zero cursor starts at the oldest environment.
type EnvsCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// MaxEnvsPerPage is the most environments a page of ListEnvs holds.
const MaxEnvsPerPage = 50

// ListEnvs gives a page of environments, oldest first, starting after cursor,
// and the cursor for the next page, or nil when this page is the last.
func (s *Store) ListEnvs(ctx context.Context, cursor *EnvsCursor) ([]database.Environment, *EnvsCursor, error) {
	params := database.ListEnvsParams{
		Limit: MaxEnvsPerPage + 1,
	}
	if cursor != nil && !cursor.CreatedAt.IsZero() {
		params.CreatedAt = sql.NullTime{Time: cursor.CreatedAt, Valid: true}
		params.ID = &cursor.ID
	}
	envs, err := s.Queries.ListEnvs(ctx, params)
	if err != nil {
		return nil, nil, ffserrors.FromDBErr(err, "can't list environments")
	}
	var next *EnvsCursor
	// The query asks for one more than a page to know whether another follows.
	if len(envs) > MaxEnvsPerPage {
		last := envs[MaxEnvsPerPage-1]
		next = &EnvsCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		envs = envs[:MaxEnvsPerPage]
	}
	return envs, next, nil
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
		return q.ChangeLogEnvironmentDeleted(ctx, database.ChangeLogEnvironmentDeletedParams{
			EnvironmentID: e.ID,
			Version:       e.Version + 1,
		})
	})
	if err != nil {
		return database.Environment{}, ffserrors.FromDBErr(err, "can't delete the environment")
	}
	return env, nil
}
