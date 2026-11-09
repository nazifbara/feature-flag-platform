package store

import (
	"bytes"
	"context"
	"encoding/json"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
)

// EnvSnapshot is an environment as an SDK syncs it from scratch: its version,
// its name and each flag's setting there, by flag key, with the flag's id and
// salt for bucketing users and the rules in position order.
type EnvSnapshot struct {
	Version      int64  `json:"version"`
	Name         string `json:"name"`
	FlagSettings map[string]struct {
		ID          uuid.UUID `json:"id"`
		Salt        string    `json:"salt"`
		Enabled     bool      `json:"enabled"`
		Fallthrough float32   `json:"fallthrough"`
		Rules       []struct {
			ID            uuid.UUID      `json:"id"`
			Serve         float32        `json:"serve"`
			StoredClauses []storedClause `json:"stored_clauses"`
		}
	} `json:"flag_settings"`
}

// SnapshotEnv gives the environment's snapshot, read in one statement so its
// version and settings agree. An SDK at that version catches up from
// ChangeSince.
func (s *Store) SnapshotEnv(ctx context.Context, id uuid.UUID) (EnvSnapshot, error) {
	var snapshot EnvSnapshot
	rawSnapshot, err := s.Queries.SnapshotEnv(ctx, id)
	if err != nil {
		return EnvSnapshot{}, ffserrors.FromDBErr(err, "can't get environment's snapshot")
	}
	jsonDecoder := json.NewDecoder(bytes.NewReader(rawSnapshot))
	if err := jsonDecoder.Decode(&snapshot); err != nil {
		return EnvSnapshot{}, connect.NewError(connect.CodeInternal, "malformed snapshot").WithCause(err)
	}
	return snapshot, nil
}
