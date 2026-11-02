package store

import (
	"context"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
)

// checkFallthrough checks f is a fraction of traffic, in [0, 1]. action
// ("create", "update") goes into the error message.
func checkFallthrough(action string, f float32) error {
	// NaN fails every comparison, so it is rejected too.
	if !(f >= 0 && f <= 1) {
		return connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a flag setting with a fallthrough outside [0, 1]")
	}
	return nil
}

// CreateFlagSetting adds a flag's settings in one environment.
func (s *Store) CreateFlagSetting(ctx context.Context, arg database.CreateFlagSettingParams) (database.FlagSetting, error) {
	if err := checkFallthrough("create", arg.Fallthrough); err != nil {
		return database.FlagSetting{}, err
	}
	setting, err := s.Queries.CreateFlagSetting(ctx, arg)
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err)
	}
	return setting, nil
}

// GetFlagSetting gets a flag's settings in one environment.
func (s *Store) GetFlagSetting(ctx context.Context, arg database.GetFlagSettingParams) (database.FlagSetting, error) {
	setting, err := s.Queries.GetFlagSetting(ctx, arg)
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err)
	}
	return setting, nil
}

// UpdateFlagSetting replaces a flag's settings in one environment, applying
// the same fallthrough rule as CreateFlagSetting.
func (s *Store) UpdateFlagSetting(ctx context.Context, arg database.UpdateFlagSettingParams) (database.FlagSetting, error) {
	if err := checkFallthrough("update", arg.Fallthrough); err != nil {
		return database.FlagSetting{}, err
	}
	setting, err := s.Queries.UpdateFlagSetting(ctx, arg)
	if err != nil {
		return database.FlagSetting{}, ffserrors.FromDBErr(err)
	}
	return setting, nil
}

// ListSettingsByFlag gets flag's settings in all environments, ordered
// by when each environment was created.
func (s *Store) ListSettingsByFlag(ctx context.Context, flagID uuid.UUID) ([]database.FlagSetting, error) {
	settings, err := s.Queries.ListSettingsByFlag(ctx, flagID)
	if err != nil {
		return nil, ffserrors.FromDBErr(err)
	}
	return settings, nil
}

// ListSettingsByEnv gets an environment's settings for all flags, each with
// its flag's key, ordered by key.
func (s *Store) ListSettingsByEnv(ctx context.Context, environmentID uuid.UUID) ([]database.ListSettingsByEnvRow, error) {
	settings, err := s.Queries.ListSettingsByEnv(ctx, environmentID)
	if err != nil {
		return nil, ffserrors.FromDBErr(err)
	}
	return settings, nil
}
