package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/lib/pq"
	"github.com/nazifbara/flagsvc/admin-api/internal/database"
)

// mustChangeSetting changes the flag's setting in env n times, each change
// moving env up one version and logging one entry.
func mustChangeSetting(t *testing.T, store *Store, flag database.Flag, env database.Environment, n int) {
	t.Helper()
	for i := range n {
		mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{
			FlagID:        flag.ID,
			EnvironmentID: env.ID,
			Enabled:       i%2 == 0,
			Fallthrough:   float32(i+1) / float32(n+1),
		})
	}
}

// mustChangeSince gives what ChangeSince gives for env since version.
func mustChangeSince(t *testing.T, store *Store, env uuid.UUID, version int64) []database.ChangeLog {
	t.Helper()
	got, err := store.ChangeSince(context.Background(), database.ChangeSinceParams{EnvironmentID: env, Version: version})
	if err != nil {
		t.Fatalf("ChangeSince(%d): %v", version, err)
	}
	return got
}

// SDKs replay the entries one after the other, so they must get every entry
// past their version, oldest first, and only those of their environment.
func TestChangeSince(t *testing.T) {
	store := newTestStore(t)

	t.Run("nothing for an SDK up to date", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		mustChangeSetting(t, store, mustCreateFlag(t, store, "new-checkout"), env, 2)
		env = mustGetEnv(t, store, env.ID)
		if got := mustChangeSince(t, store, env.ID, env.Version); len(got) != 0 {
			t.Fatalf("ChangeSince(%d) = %+v, want no entries", env.Version, got)
		}
	})

	// Replaying in any other order would leave the SDK with an older setting,
	// or with rules in the wrong places.
	t.Run("entries past the version, oldest first", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		flag := mustCreateFlag(t, store, "new-checkout")
		since := mustGetEnv(t, store, env.ID).Version
		mustChangeSetting(t, store, flag, env, 5)

		want := queryChangeLogs(t, store, "environment_id = $1 AND target_version > $2", env.ID, since)
		if len(want) != 5 {
			t.Fatalf("%d entries past version %d, want 5", len(want), since)
		}
		if diff := cmp.Diff(want, mustChangeSince(t, store, env.ID, since)); diff != "" {
			t.Fatalf("ChangeSince(%d) (-want +got):\n%s", since, diff)
		}
	})

	t.Run("only the environment's entries", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		other := mustCreateEnv(t, store, "staging")
		flag := mustCreateFlag(t, store, "new-checkout")
		mustChangeSetting(t, store, flag, other, 3)

		got := mustChangeSince(t, store, env.ID, 1)
		want := queryChangeLogs(t, store, "environment_id = $1", env.ID)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("ChangeSince(1) (-want +got):\n%s", diff)
		}
	})

	// The furthest an SDK can be behind and still catch up: every entry it is
	// missing comes back, none cut off.
	t.Run("every entry for an SDK as far behind as allowed", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		flag := mustCreateFlag(t, store, "new-checkout")
		since := mustGetEnv(t, store, env.ID).Version
		mustChangeSetting(t, store, flag, env, maxChangesBehind)

		got := mustChangeSince(t, store, env.ID, since)
		want := queryChangeLogs(t, store, "environment_id = $1 AND target_version > $2", env.ID, since)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("ChangeSince(%d) (-want +got):\n%s", since, diff)
		}
		if len(got) != maxChangesBehind {
			t.Fatalf("ChangeSince(%d) gave %d entries, want %d", since, len(got), maxChangesBehind)
		}
	})

	// One version too many and the SDK has to sync from scratch instead.
	t.Run("too old an SDK one version further behind", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		flag := mustCreateFlag(t, store, "new-checkout")
		since := mustGetEnv(t, store, env.ID).Version
		mustChangeSetting(t, store, flag, env, maxChangesBehind+1)

		got, err := store.ChangeSince(context.Background(), database.ChangeSinceParams{EnvironmentID: env.ID, Version: since})
		wantConnectErr(t, err, connect.NewError(connect.CodeFailedPrecondition, "too old"))
		if got != nil {
			t.Fatalf("ChangeSince(%d) = %+v on error, want nil", since, got)
		}
	})

	// An SDK can't be ahead of the server, so its version is wrong.
	t.Run("rejects a version past the environment's", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		got, err := store.ChangeSince(context.Background(), database.ChangeSinceParams{EnvironmentID: env.ID, Version: env.Version + 1})
		connectErr, ok := errors.AsType[*connect.Error](err)
		if !ok || connectErr.Code() != connect.CodeInvalidArgument {
			t.Fatalf("ChangeSince(%d) error = %v, want code %v", env.Version+1, err, connect.CodeInvalidArgument)
		}
		if got != nil {
			t.Fatalf("ChangeSince(%d) = %+v on error, want nil", env.Version+1, got)
		}
	})

	t.Run("not found for an unknown environment", func(t *testing.T) {
		resetDB(t, store.db)
		got, err := store.ChangeSince(context.Background(), database.ChangeSinceParams{EnvironmentID: uuid.New(), Version: 1})
		wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't get the change logs"))
		if got != nil {
			t.Fatalf("ChangeSince() = %+v on error, want nil", got)
		}
	})
}

// ChangeSince reads the version and the entries under the lock, so it can't see
// a version a write has bumped but not logged yet.
func TestChangeSinceWaitsForMatrixLock(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "production")
	tx, _ := beginMatrixTx(t, store)
	defer tx.Rollback()

	done := wantWaits(t, func() error {
		_, err := store.ChangeSince(context.Background(), database.ChangeSinceParams{EnvironmentID: env.ID, Version: env.Version})
		return err
	})
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(): %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ChangeSince(): %v", err)
	}
}

func TestChangeSinceReportsErrors(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "production")
	arg := database.ChangeSinceParams{EnvironmentID: env.ID, Version: env.Version}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, "can't get the change logs"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't get the change logs"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ChangeSince(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			if got != nil {
				t.Fatalf("ChangeSince() = %+v on error, want nil", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.ChangeSince(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't get the change logs"))
		if got != nil {
			t.Fatalf("ChangeSince() = %+v on error, want nil", got)
		}
	})
}

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
// Only a bug gets there, so the client is told it's internal, not that what it
// asked for exists.
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
	wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag setting"))
	if got != (database.FlagSetting{}) {
		t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
	}
	if got := matrixState(t, store); got != want {
		t.Fatalf("state = %s, want %s", got, want)
	}
}

// Evaluating a flag takes its id and salt, and a rule its id, so the entries
// creating them must carry them, and only those: the others identify flags by
// key and rules by position.
func TestChangeLogsTableIdentityFields(t *testing.T) {
	store := newTestStore(t)
	const checkViolation = "23514"
	const flagFields, ruleFields = "flag_identity_fields", "rule_identity_fields"
	flagID, ruleID := uuid.New(), uuid.New()

	// Each entry has what its action otherwise requires, so only the
	// identity fields decide.
	tests := map[string]struct {
		action                   string
		flagID, ruleID           *uuid.UUID
		salt                     sql.NullString
		enabled, fall, serve     any
		clauses                  any
		oldPosition, newPosition any
		want                     string // "" when the entry is taken
	}{
		"flag-created with id and salt":    {action: "flag-created", flagID: &flagID, salt: sql.NullString{String: "s", Valid: true}, enabled: true, fall: 0},
		"flag-created without id":          {action: "flag-created", salt: sql.NullString{String: "s", Valid: true}, enabled: true, fall: 0, want: flagFields},
		"flag-created without salt":        {action: "flag-created", flagID: &flagID, enabled: true, fall: 0, want: flagFields},
		"setting-updated with a flag id":   {action: "setting-updated", flagID: &flagID, enabled: true, fall: 0, want: flagFields},
		"setting-updated with a salt":      {action: "setting-updated", salt: sql.NullString{String: "s", Valid: true}, enabled: true, fall: 0, want: flagFields},
		"rule-created with an id":          {action: "rule-created", ruleID: &ruleID, serve: 1, clauses: validClauses, newPosition: 0},
		"rule-created without an id":       {action: "rule-created", serve: 1, clauses: validClauses, newPosition: 0, want: ruleFields},
		"rule-updated with a rule id":      {action: "rule-updated", ruleID: &ruleID, serve: 1, clauses: validClauses, newPosition: 0, want: ruleFields},
		"rule-position-changed with an id": {action: "rule-position-changed", ruleID: &ruleID, oldPosition: 0, newPosition: 1, want: ruleFields},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			_, err := store.db.Exec(`
				INSERT INTO change_logs (id, target_version, environment_id, flag_new_key, action, created_at,
				  flag_id, flag_salt, rule_id, enabled, fallthrough, rule_serve, rule_clauses,
				  rule_old_position, rule_new_position)
				VALUES (gen_random_uuid(), 2, gen_random_uuid(), 'new-checkout', $1, now(),
				  $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				test.action, test.flagID, test.salt, test.ruleID, test.enabled, test.fall,
				test.serve, test.clauses, test.oldPosition, test.newPosition)
			if test.want == "" {
				if err != nil {
					t.Fatalf("inserting the entry: %v", err)
				}
				return
			}
			var pqErr *pq.Error
			if !errors.As(err, &pqErr) || string(pqErr.Code) != checkViolation || pqErr.Constraint != test.want {
				t.Fatalf("inserting the entry: error = %v, want %s to reject it", err, test.want)
			}
			if n := changeLogCount(t, store); n != 0 {
				t.Fatalf("%d change logs after a rejected insert, want 0", n)
			}
		})
	}
}
