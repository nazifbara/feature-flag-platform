// Package flagsetting serves the admin API for flag settings: whether a flag is
// on in one environment, and what fraction of traffic it serves there.
package flagsetting

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/utils"
	flagsettingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the FlagSettingService RPCs on top of the store.
type Service struct {
	store *store.Store
}

// New returns a Service that keeps its data in store.
func New(store *store.Store) *Service {
	return &Service{store: store}
}

// settingToProto converts a stored flag setting into the API's FlagSetting.
func settingToProto(storedSetting database.FlagSetting) *flagsettingv1.FlagSetting {
	return &flagsettingv1.FlagSetting{
		Id:            storedSetting.ID.String(),
		FlagId:        storedSetting.FlagID.String(),
		EnvironmentId: storedSetting.EnvironmentID.String(),
		Enabled:       storedSetting.Enabled,
		Fallthrough:   storedSetting.Fallthrough,
		CreatedAt:     timestamppb.New(storedSetting.CreatedAt),
		UpdatedAt:     timestamppb.New(storedSetting.UpdatedAt),
	}
}

// Get returns the flag setting with the requested id. The proto's rules reject
// an id that isn't a UUID; parsing it again here only guards against a caller
// without them. An id no setting has gives NotFound.
func (s *Service) Get(ctx context.Context, req *flagsettingv1.GetRequest) (*flagsettingv1.GetResponse, error) {
	settingID, err := utils.ParseID(req.Id, "flag setting")
	if err != nil {
		return nil, err
	}
	setting, err := s.store.GetFlagSettingByID(ctx, settingID)
	if err != nil {
		return nil, err
	}
	return &flagsettingv1.GetResponse{FlagSetting: settingToProto(setting)}, nil
}

// Update gives the flag setting with the requested id the values its update
// mask names, checked by the store, keeping the others whatever the request
// holds for them, and returns the setting as stored. An empty mask changes
// nothing. The proto's rules reject an id that isn't a UUID; parsing it again
// here only guards against a caller without them. Store errors reach the client
// as they are.
func (s *Service) Update(ctx context.Context, req *flagsettingv1.UpdateRequest) (*flagsettingv1.UpdateResponse, error) {
	settingID, err := utils.ParseID(req.Id, "flag setting")
	if err != nil {
		return nil, err
	}
	update := store.FlagSettingUpdate{ID: settingID}
	// The proto's rules reject other paths, but also take sub-paths such as
	// enabled.x, so each path is matched exactly here.
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "enabled":
			update.Enabled = &req.Enabled
		case "fallthrough":
			update.Fallthrough = &req.Fallthrough
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid update mask path %q", path))
		}
	}
	setting, err := s.store.UpdateFlagSetting(ctx, update)
	if err != nil {
		return nil, err
	}
	return &flagsettingv1.UpdateResponse{FlagSetting: settingToProto(setting)}, nil
}
