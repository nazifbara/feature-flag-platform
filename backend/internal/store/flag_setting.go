package store

import (
	"context"

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
