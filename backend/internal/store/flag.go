package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
	"github.com/nazifbara/flagsvc/backend/internal/utils"
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

// CreateFlag adds a flag, with its key normalized by normalizeFlagKey and a salt
// of its own, and a disabled setting in every existing environment, whose
// versions it bumps and whose change logs record it, in one transaction.
func (s *Store) CreateFlag(ctx context.Context, key string) (database.Flag, error) {
	key, err := normalizeFlagKey("create", key)
	if err != nil {
		return database.Flag{}, err
	}
	var flag database.Flag
	err = s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		f, err := q.CreateFlag(
			ctx,
			database.CreateFlagParams{Key: key, Salt: utils.GenerateSalt(16)},
		)
		if err != nil {
			return err
		}
		flag = f
		if err := q.CreateSettingsForFlag(ctx, f.ID); err != nil {
			return err
		}
		if err := q.IncrementAllEnvVersions(ctx); err != nil {
			return err
		}
		return q.CreateChangeLogsForFlag(ctx, f.ID)
	})
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't create the flag")
	}
	return flag, nil
}

// GetFlag gets the flag with the given id.
func (s *Store) GetFlag(ctx context.Context, id uuid.UUID) (database.Flag, error) {
	flag, err := s.Queries.GetFlag(ctx, id)
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't get the flag")
	}
	return flag, nil
}

// UpdateFlag changes the key of the flag with the given id, applying the same
// key rules as CreateFlag, and bumps the version of every environment, in one
// transaction, unless the flag already has that key, which changes nothing.
func (s *Store) UpdateFlag(ctx context.Context, id uuid.UUID, key string) (database.Flag, error) {
	key, err := normalizeFlagKey("update", key)
	if err != nil {
		return database.Flag{}, err
	}
	var flag database.Flag
	err = s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		currentFlag, err := q.GetFlag(ctx, id)
		if err != nil {
			return err
		}
		if currentFlag.Key == key {
			flag = currentFlag
			return nil
		}
		f, err := q.UpdateFlag(ctx, database.UpdateFlagParams{ID: id, Key: key})
		if err != nil {
			return err
		}
		flag = f
		return q.IncrementAllEnvVersions(ctx)
	})
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't update the flag")
	}
	return flag, err
}

// DeleteFlag deletes the flag with the given id, along with its settings in
// every environment, whose versions it bumps, and returns the flag as it was.
func (s *Store) DeleteFlag(ctx context.Context, id uuid.UUID) (database.Flag, error) {
	var flag database.Flag
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		f, err := q.DeleteFlag(ctx, id)
		if err != nil {
			return err
		}
		flag = f
		return q.IncrementAllEnvVersions(ctx)
	})
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't delete the flag")
	}
	return flag, nil
}
