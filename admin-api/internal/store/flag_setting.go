package store

import (
	"context"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/ffserrors"
)

// checkFallthrough checks f is a fraction of traffic, in [0, 1].
func checkFallthrough(f float32) error {
	// NaN fails every comparison, so it is rejected too.
	if !(f >= 0 && f <= 1) {
		return connect.NewError(connect.CodeInvalidArgument, "can't update a flag setting with a fallthrough outside [0, 1]")
	}
	return nil
}

// GetFlagSetting gets a flag's settings in one environment.
func (s *Store) GetFlagSetting(ctx context.Context, arg database.GetFlagSettingParams) (database.FlagSetting, error) {
	setting, err := s.Queries.GetFlagSetting(ctx, arg)
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err, "can't get the flag setting")
	}
	return setting, nil
}

// FlagSettingUpdate holds the values to give the flag setting with ID. A nil
// field keeps the value the setting has.
type FlagSettingUpdate struct {
	ID          uuid.UUID
	Enabled     *bool
	Fallthrough *float32
}

// GetFlagSettingByID gets the flag setting with id. An id no setting has gives
// NotFound.
func (s *Store) GetFlagSettingByID(ctx context.Context, id uuid.UUID) (database.FlagSetting, error) {
	setting, err := s.Queries.GetFlagSettingByID(ctx, id)
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err, "can't get the flag setting")
	}
	return setting, nil
}

// UpdateFlagSetting gives the flag setting with arg.ID the values arg sets,
// with its fallthrough checked by checkFallthrough, and bumps its environment's
// version. The values left unset are read in the same transaction, so a racing
// update's values aren't overwritten with stale ones. Values the setting
// already has, or none at all, change nothing, and it comes back as it is. An
// id no setting has gives NotFound, and a setting of an archived flag gives
// FailedPrecondition.
func (s *Store) UpdateFlagSetting(ctx context.Context, arg FlagSettingUpdate) (database.FlagSetting, error) {
	if arg.Fallthrough != nil {
		if err := checkFallthrough(*arg.Fallthrough); err != nil {
			return database.FlagSetting{}, err
		}
	}
	var setting database.FlagSetting
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		currentSetting, err := q.GetFlagSettingByID(ctx, arg.ID)
		if err != nil {
			return err
		}
		flag, err := q.GetFlag(ctx, currentSetting.FlagID)
		if err != nil {
			return err
		}
		if flag.Archived {
			return connect.NewError(connect.CodeFailedPrecondition, "can't update archived flag")
		}
		update := database.UpdateFlagSettingParams{
			ID:          currentSetting.ID,
			Enabled:     currentSetting.Enabled,
			Fallthrough: currentSetting.Fallthrough,
		}
		if arg.Enabled != nil {
			update.Enabled = *arg.Enabled
		}
		if arg.Fallthrough != nil {
			update.Fallthrough = *arg.Fallthrough
		}
		if currentSetting.Enabled == update.Enabled && currentSetting.Fallthrough == update.Fallthrough {
			setting = currentSetting
			return nil
		}
		stg, err := q.UpdateFlagSetting(ctx, update)
		if err != nil {
			return err
		}
		setting = stg
		if err := q.IncrementEnvVersion(ctx, stg.EnvironmentID); err != nil {
			return err
		}
		return q.ChangeLogSettingUpdated(ctx, stg.ID)
	})
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err, "can't update the flag setting")
	}
	return setting, nil
}

// ListSettingsByFlag gets flag's settings in all environments, ordered
// by when each environment was created.
func (s *Store) ListSettingsByFlag(ctx context.Context, flagID uuid.UUID) ([]database.FlagSetting, error) {
	settings, err := s.Queries.ListSettingsByFlag(ctx, flagID)
	if err != nil {
		return nil, ffserrors.FromDBErr(err, "can't list the flag's settings")
	}
	return settings, nil
}

// ListSettingsByEnv gets an environment's settings for all flags, each with
// its flag's key, ordered by key.
func (s *Store) ListSettingsByEnv(ctx context.Context, environmentID uuid.UUID) ([]database.ListSettingsByEnvRow, error) {
	settings, err := s.Queries.ListSettingsByEnv(ctx, environmentID)
	if err != nil {
		return nil, ffserrors.FromDBErr(err, "can't list the environment's settings")
	}
	return settings, nil
}
