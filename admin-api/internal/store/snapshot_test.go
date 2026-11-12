package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
)

// snapshotRule and snapshotSetting mirror a snapshot's settings in named types,
// so tests can spell out what they want.
type snapshotRule struct {
	ID      uuid.UUID
	Serve   float32
	Clauses []storedClause
}

type snapshotSetting struct {
	ID          uuid.UUID
	Salt        string
	Enabled     bool
	Fallthrough float32
	Rules       []snapshotRule
}

// settingsOf gives the snapshot's settings by flag key.
func settingsOf(snapshot EnvSnapshot) map[string]snapshotSetting {
	settings := make(map[string]snapshotSetting, len(snapshot.FlagSettings))
	for key, s := range snapshot.FlagSettings {
		setting := snapshotSetting{ID: s.ID, Salt: s.Salt, Enabled: s.Enabled, Fallthrough: s.Fallthrough}
		for _, r := range s.Rules {
			setting.Rules = append(setting.Rules, snapshotRule{ID: r.ID, Serve: r.Serve, Clauses: r.StoredClauses})
		}
		settings[key] = setting
	}
	return settings
}

// storedClausesOf decodes clauses the way they're stored.
func storedClausesOf(t *testing.T, clauses string) []storedClause {
	t.Helper()
	var stored []storedClause
	if err := json.Unmarshal([]byte(clauses), &stored); err != nil {
		t.Fatalf("decoding clauses %s: %v", clauses, err)
	}
	return stored
}

func mustSnapshotEnv(t *testing.T, store *Store, id uuid.UUID) EnvSnapshot {
	t.Helper()
	snapshot, err := store.SnapshotEnv(context.Background(), id)
	if err != nil {
		t.Fatalf("SnapshotEnv(): %v", err)
	}
	return snapshot
}

// An SDK syncing from scratch gets everything evaluating the environment's
// flags takes, at the environment's version.
func TestSnapshotEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("the environment's version and name", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		mustChangeSetting(t, store, mustCreateFlag(t, store, "new-checkout"), env, 3)
		env = mustGetEnv(t, store, env.ID)

		got := mustSnapshotEnv(t, store, env.ID)
		if got.Version != env.Version || got.Name != env.Name {
			t.Fatalf("SnapshotEnv() version, name = %d, %q, want %d, %q", got.Version, got.Name, env.Version, env.Name)
		}
	})

	// Rules come in position order, the order they're evaluated in and the
	// one change log positions count in.
	t.Run("every flag's setting by key, rules in order", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		checkout := mustCreateFlag(t, store, "new-checkout")
		dark := mustCreateFlag(t, store, "dark-mode")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: checkout.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		last := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: s.ID, Name: "last", Serve: 1, Clauses: json.RawMessage(validClauses)})
		first := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: s.ID, Name: "first", Position: 0, Serve: 0.25, Clauses: json.RawMessage(otherClauses)})

		want := map[string]snapshotSetting{
			"new-checkout": {ID: checkout.ID, Salt: checkout.Salt, Enabled: true, Fallthrough: 0.5, Rules: []snapshotRule{
				{ID: first.ID, Serve: 0.25, Clauses: storedClausesOf(t, otherClauses)},
				{ID: last.ID, Serve: 1, Clauses: storedClausesOf(t, validClauses)},
			}},
			"dark-mode": {ID: dark.ID, Salt: dark.Salt, Enabled: false, Fallthrough: 0},
		}
		got := settingsOf(mustSnapshotEnv(t, store, env.ID))
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("SnapshotEnv() settings (-want +got):\n%s", diff)
		}
	})

	t.Run("only the environment's settings", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		other := mustCreateEnv(t, store, "staging")
		flag := mustCreateFlag(t, store, "new-checkout")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: other.ID, Enabled: true, Fallthrough: 1})
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: s.ID, Name: "a", Serve: 1, Clauses: json.RawMessage(validClauses)})

		want := map[string]snapshotSetting{"new-checkout": {ID: flag.ID, Salt: flag.Salt}}
		got := settingsOf(mustSnapshotEnv(t, store, env.ID))
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("SnapshotEnv() settings (-want +got):\n%s", diff)
		}
	})

	// Nothing to evaluate is still a snapshot, not an error.
	t.Run("an environment without flags", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		got := mustSnapshotEnv(t, store, env.ID)
		if got.Version != env.Version || got.Name != env.Name || len(got.FlagSettings) != 0 {
			t.Fatalf("SnapshotEnv() = %+v, want version %d, name %q and no settings", got, env.Version, env.Name)
		}
	})

	t.Run("not found for an unknown environment", func(t *testing.T) {
		resetDB(t, store.db)
		got, err := store.SnapshotEnv(ctx, uuid.New())
		wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't get environment's snapshot"))
		if got.Version != 0 || got.Name != "" || got.FlagSettings != nil {
			t.Fatalf("SnapshotEnv() = %+v on error, want the zero EnvSnapshot", got)
		}
	})

	// The clauses check only holds a list, so what's in it can still be wrong
	// for storedClause; that's corrupt data, not the caller's mistake. What
	// failed to decode is kept as the cause, for the logs.
	t.Run("internal for clauses that don't decode", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 1, Clauses: json.RawMessage(validClauses)})
		if _, err := store.db.Exec(`UPDATE rules SET clauses = '[{"attribute": 5}]' WHERE id = $1`, rule.ID); err != nil {
			t.Fatalf("corrupting the clauses: %v", err)
		}
		_, err := store.SnapshotEnv(ctx, setting.EnvironmentID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "malformed snapshot"))
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok {
			t.Fatalf("SnapshotEnv() error = %v, want the decode error as its cause", err)
		}
	})
}

// replaySettings applies change log entries to settings the way an SDK would,
// in version order, and gives the result. It takes from each entry only what
// the entry carries, so a flag or rule whose entry left out what evaluating it
// takes comes out without it.
func replaySettings(t *testing.T, settings map[string]snapshotSetting, logs []database.ChangeLog) map[string]snapshotSetting {
	t.Helper()
	settings = cloneSettings(settings)
	for _, l := range logs {
		key := l.FlagNewKey.String
		s := settings[key]
		pos := int(l.RuleNewPosition.Int32)
		switch l.Action {
		case "flag-created":
			var id uuid.UUID
			if l.FlagID != nil {
				id = *l.FlagID
			}
			settings[key] = snapshotSetting{ID: id, Salt: l.FlagSalt.String, Enabled: l.Enabled.Bool, Fallthrough: float32(l.Fallthrough.Float64)}
		case "flag-updated":
			settings[key] = settings[l.FlagOldKey.String]
			delete(settings, l.FlagOldKey.String)
		case "flag-deleted":
			delete(settings, key)
		case "setting-updated":
			s.Enabled, s.Fallthrough = l.Enabled.Bool, float32(l.Fallthrough.Float64)
			settings[key] = s
		case "rule-created":
			var id uuid.UUID
			if l.RuleID != nil {
				id = *l.RuleID
			}
			s.Rules = slices.Insert(s.Rules, pos, snapshotRule{ID: id, Serve: float32(l.RuleServe.Float64), Clauses: storedClausesOf(t, string(*l.RuleClauses))})
			settings[key] = s
		case "rule-updated":
			s.Rules[pos].Serve, s.Rules[pos].Clauses = float32(l.RuleServe.Float64), storedClausesOf(t, string(*l.RuleClauses))
			settings[key] = s
		case "rule-deleted":
			s.Rules = slices.Delete(s.Rules, pos, pos+1)
			settings[key] = s
		case "rule-position-changed":
			old := int(l.RuleOldPosition.Int32)
			moved := s.Rules[old]
			s.Rules = slices.Insert(slices.Delete(s.Rules, old, old+1), pos, moved)
			settings[key] = s
		default:
			t.Fatalf("entry at version %d has action %q, which a snapshot can't replay", l.TargetVersion, l.Action)
		}
	}
	return settings
}

// cloneSettings copies settings deeply enough for replaySettings to change it.
func cloneSettings(settings map[string]snapshotSetting) map[string]snapshotSetting {
	out := make(map[string]snapshotSetting, len(settings))
	for k, s := range settings {
		s.Rules = slices.Clone(s.Rules)
		out[k] = s
	}
	return out
}

// An SDK starts from a snapshot and catches up from the change log, so the
// snapshot at its version plus the entries past it must be the snapshot it
// would get now.
func TestSnapshotEnvAndChangeLogAgree(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	env := mustCreateEnv(t, store, "production")
	checkout := mustCreateFlag(t, store, "new-checkout")
	gone := mustCreateFlag(t, store, "gone")
	setting, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: checkout.ID, EnvironmentID: env.ID})
	if err != nil {
		t.Fatalf("GetFlagSetting(): %v", err)
	}
	rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
	before := mustSnapshotEnv(t, store, env.ID)

	writes := []func() error{
		func() error {
			_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: checkout.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.75})
			return err
		},
		func() error {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "d", Position: 1, Serve: 0.5, Clauses: json.RawMessage(otherClauses)})
			return err
		},
		func() error {
			_, err := store.UpdateRule(ctx, database.UpdateRuleParams{ID: rules[2].ID, Name: "c", Serve: 0.125, Clauses: json.RawMessage(otherClauses)})
			return err
		},
		func() error {
			_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 3})
			return err
		},
		func() error { _, err := store.DeleteRule(ctx, rules[1].ID); return err },
		func() error { _, err := store.UpdateFlag(ctx, checkout.ID, "checkout-v2"); return err },
		func() error { _, err := store.DeleteFlag(ctx, gone.ID); return err },
		func() error { _, err := store.CreateFlag(ctx, "fresh"); return err },
	}
	for i, write := range writes {
		if err := write(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	after := mustSnapshotEnv(t, store, env.ID)
	got := replaySettings(t, settingsOf(before), mustChangeSince(t, store, env.ID, before.Version))
	if diff := cmp.Diff(settingsOf(after), got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("snapshot at version %d plus the change log (-snapshot now +replayed):\n%s", before.Version, diff)
	}
}

func TestSnapshotEnvReportsErrors(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "production")

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
			want: connect.NewError(connect.CodeCanceled, "can't get environment's snapshot"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't get environment's snapshot"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := store.SnapshotEnv(test.ctx, env.ID)
			wantConnectErr(t, err, test.want)
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		_, err := store.SnapshotEnv(context.Background(), env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't get environment's snapshot"))
	})
}
