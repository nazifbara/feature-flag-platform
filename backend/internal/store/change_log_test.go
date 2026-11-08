package store

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/lib/pq"
	"github.com/nazifbara/flagsvc/backend/internal/database"
)

// An environment's versions and its entries go one to one, which is what keeps
// ChangeSince to maxChangesBehind entries, so the table takes at most one entry
// per environment and version.
func TestChangeLogsTableRejectsSameVersionTwice(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const uniqueViolation = "23505"
	env, other := uuid.New(), uuid.New()

	log := func(env uuid.UUID, version int64) error {
		return store.Queries.CreateChangeLogsForEnvDelete(ctx, database.CreateChangeLogsForEnvDeleteParams{Version: version, EnvironmentID: env})
	}
	if err := log(env, 2); err != nil {
		t.Fatalf("logging version 2: %v", err)
	}

	var pqErr *pq.Error
	if err := log(env, 2); !errors.As(err, &pqErr) || string(pqErr.Code) != uniqueViolation {
		t.Fatalf("logging version 2 again: error = %v, want code %s", err, uniqueViolation)
	}
	if n := changeLogCount(t, store); n != 1 {
		t.Fatalf("%d change logs after a rejected insert, want 1", n)
	}

	// Versions are counted per environment, so others can use the same one.
	if err := log(other, 2); err != nil {
		t.Fatalf("logging version 2 in another environment: %v", err)
	}
	if err := log(env, 3); err != nil {
		t.Fatalf("logging version 3: %v", err)
	}
}

// A write whose entry would take a version already logged fails and changes
// nothing, so an environment never ends up with two entries for one version.
func TestWriteLoggingATakenVersionFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	env := mustCreateEnv(t, store, "production")
	flag := mustCreateFlag(t, store, "new-checkout")
	env = mustGetEnv(t, store, env.ID)
	if err := store.Queries.CreateChangeLogsForEnvDelete(ctx, database.CreateChangeLogsForEnvDeleteParams{Version: env.Version + 1, EnvironmentID: env.ID}); err != nil {
		t.Fatalf("taking version %d: %v", env.Version+1, err)
	}
	want := matrixState(t, store)

	got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
	if err == nil {
		t.Fatalf("UpdateFlagSetting() = %+v, want an error", got)
	}
	if got := matrixState(t, store); got != want {
		t.Fatalf("state = %s, want %s", got, want)
	}
}
