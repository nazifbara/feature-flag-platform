package store

import (
	"context"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
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

// UpdateFlagSetting replaces a flag's settings in one environment, with its
// fallthrough checked by checkFallthrough, and bumps that environment's version.
// Values the setting already has change nothing, and it comes back as it is.
func (s *Store) UpdateFlagSetting(ctx context.Context, arg database.UpdateFlagSettingParams) (database.FlagSetting, error) {
	if err := checkFallthrough(arg.Fallthrough); err != nil {
		return database.FlagSetting{}, err
	}
	var setting database.FlagSetting
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		currentSetting, err := q.GetFlagSetting(ctx, database.GetFlagSettingParams{
			EnvironmentID: arg.EnvironmentID,
			FlagID:        arg.FlagID,
		})
		if err != nil {
			return err
		}
		if currentSetting.Enabled == arg.Enabled && currentSetting.Fallthrough == arg.Fallthrough {
			setting = currentSetting
			return nil
		}
		stg, err := q.UpdateFlagSetting(ctx, arg)
		if err != nil {
			return err
		}
		setting = stg
		if err := q.IncrementEnvVersion(ctx, stg.EnvironmentID); err != nil {
			return err
		}
		return q.CreateChangeLogsForSettingUpdate(ctx, stg.ID)
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
