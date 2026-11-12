package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/ffserrors"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/utils"
)

// FlagsCursor marks where a page of ListFlags ended: the next page starts at
// the first key after Key. The zero cursor starts at the first flag.
type FlagsCursor struct {
	Key string `json:"key"`
}

// MaxFlagsPerPage is the most flags a page of ListFlags holds.
const MaxFlagsPerPage = 50

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
		return q.ChangeLogFlagCreated(ctx, f.ID)
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
// key rules as CreateFlag, bumps the version of every environment and logs the
// rename there, in one transaction, unless the flag already has that key,
// which changes nothing.
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
		if err := q.IncrementAllEnvVersions(ctx); err != nil {
			return err
		}
		return q.ChangeLogFlagUpdated(ctx, database.ChangeLogFlagUpdatedParams{OldKey: currentFlag.Key, FlagID: f.ID})
	})
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't update the flag")
	}
	return flag, err
}

// DeleteFlag deletes the flag with the given id, along with its settings in
// every environment, whose versions it bumps and whose change logs record it,
// and returns the flag as it was.
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
		if err := q.IncrementAllEnvVersions(ctx); err != nil {
			return err
		}
		return q.ChangeLogFlagDeleted(ctx, f.Key)
	})
	if err != nil {
		return database.Flag{}, ffserrors.FromDBErr(err, "can't delete the flag")
	}
	return flag, nil
}

// ListFlags gives a page of flags ordered by key, starting after cursor's key,
// and the cursor for the next page, or nil when this page is the last.
func (s *Store) ListFlags(ctx context.Context, cursor *FlagsCursor) ([]database.Flag, *FlagsCursor, error) {
	params := database.ListFlagsParams{
		Limit: MaxFlagsPerPage + 1,
	}
	if cursor != nil {
		params.Key = sql.NullString{
			String: cursor.Key,
			Valid:  cursor.Key != "",
		}
	}
	flags, err := s.Queries.ListFlags(ctx, params)
	if err != nil {
		return nil, nil, ffserrors.FromDBErr(err, "can't list flags")
	}
	var nexCursor *FlagsCursor
	if len(flags) > MaxFlagsPerPage {
		nexCursor = &FlagsCursor{
			Key: flags[len(flags)-2].Key,
		}
		flags = flags[:len(flags)-1]
	}

	return flags, nexCursor, nil
}

// ArchiveFlag archives the flag with id, freezing its settings and rules until
// it's unarchived, and logs it in every environment. It returns the flag as
// stored; archiving an archived flag changes and logs nothing.
func (s *Store) ArchiveFlag(ctx context.Context, id uuid.UUID) (database.Flag, error) {
	var flag database.Flag
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		f, err := q.GetFlag(ctx, id)
		if err != nil {
			return err
		}
		if f.Archived {
			flag = f
			return nil
		}
		if err := q.ArchiveFlag(ctx, id); err != nil {
			return err
		}
		if err := q.IncrementAllEnvVersions(ctx); err != nil {
			return err
		}
		if err := q.ChangeLogFlagArchived(ctx, id); err != nil {
			return err
		}
		flag = f
		flag.Archived = true
		return nil
	})
	if err != nil {
		return flag, ffserrors.FromDBErr(err, "can't archive flag")
	}
	return flag, nil
}

// UnarchiveFlag unarchives the flag with id, so its settings and rules can
// change again, and logs it in every environment. It returns the flag as
// stored; unarchiving a flag that isn't archived changes and logs nothing.
func (s *Store) UnarchiveFlag(ctx context.Context, id uuid.UUID) (database.Flag, error) {
	var flag database.Flag
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		f, err := q.GetFlag(ctx, id)
		if err != nil {
			return err
		}
		if !f.Archived {
			flag = f
			return nil
		}
		if err := q.UnarchiveFlag(ctx, id); err != nil {
			return err
		}
		if err := q.IncrementAllEnvVersions(ctx); err != nil {
			return err
		}
		if err := q.ChangeLogFlagUnarchived(ctx, id); err != nil {
			return err
		}
		flag = f
		flag.Archived = false
		return nil
	})
	if err != nil {
		return flag, ffserrors.FromDBErr(err, "can't unarchive flag")
	}
	return flag, nil
}
