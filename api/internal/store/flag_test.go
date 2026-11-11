package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/nazifbara/flagsvc/api/internal/database"
)

// flagCount gives the number of committed flags.
func flagCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM flags").Scan(&n); err != nil {
		t.Fatalf("counting flags: %v", err)
	}
	return n
}

// storedFlag reads a flag straight from the table, so tests don't depend on
// a GetFlag.
func storedFlag(t *testing.T, store *Store, id uuid.UUID) database.Flag {
	t.Helper()
	var f database.Flag
	err := store.db.QueryRow("SELECT id, key, salt, created_at, updated_at, archived FROM flags WHERE id = $1", id).
		Scan(&f.ID, &f.Key, &f.Salt, &f.CreatedAt, &f.UpdatedAt, &f.Archived)
	if err != nil {
		t.Fatalf("reading flag %v: %v", id, err)
	}
	return f
}

func TestCreateFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("creates the flag", func(t *testing.T) {
		resetDB(t, store.db)
		before := time.Now()
		got, err := store.CreateFlag(ctx, "new-checkout")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		if got.Key != "new-checkout" {
			t.Errorf("CreateFlag().Key = %q, want %q", got.Key, "new-checkout")
		}
		if got.ID == uuid.Nil() {
			t.Error("CreateFlag() returned a zero ID")
		}
		// Evaluating a flag without a salt is an error.
		if got.Salt == "" {
			t.Error("CreateFlag() returned an empty salt")
		}
		// A new flag has never been updated.
		if !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Errorf("CreateFlag() updated_at = %v, want it equal to created_at %v", got.UpdatedAt, got.CreatedAt)
		}
		// The database and the tests share a clock closely enough for a
		// minute's slack either way.
		if d := got.CreatedAt.Sub(before); d < -time.Minute || d > time.Minute {
			t.Errorf("CreateFlag() created_at = %v, want close to %v", got.CreatedAt, before)
		}
		if diff := cmp.Diff(got, storedFlag(t, store, got.ID)); diff != "" {
			t.Fatalf("stored flag mismatch (-returned +stored):\n%s", diff)
		}
	})

	t.Run("each flag gets its own ID", func(t *testing.T) {
		resetDB(t, store.db)
		a, err := store.CreateFlag(ctx, "new-checkout")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		b, err := store.CreateFlag(ctx, "dark-mode")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		if a.ID == b.ID {
			t.Fatalf("CreateFlag() reused ID %v", a.ID)
		}
	})

	// Flags sharing a salt would roll out to the same users.
	t.Run("each flag gets its own salt", func(t *testing.T) {
		resetDB(t, store.db)
		a, err := store.CreateFlag(ctx, "new-checkout")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		b, err := store.CreateFlag(ctx, "dark-mode")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		if a.Salt == b.Salt {
			t.Fatalf("CreateFlag() reused salt %q", a.Salt)
		}
	})

	// The clock is read per flag, not per transaction, so flags created
	// together still get increasing times.
	t.Run("flags created in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		var flags []database.Flag
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, key := range []string{"a", "b", "c"} {
				f, err := q.CreateFlag(ctx, database.CreateFlagParams{Key: key, Salt: "s4lt-" + key})
				if err != nil {
					return err
				}
				flags = append(flags, f)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		for i := 1; i < len(flags); i++ {
			if !flags[i].CreatedAt.After(flags[i-1].CreatedAt) {
				t.Errorf("%q created_at %v not after %q created_at %v", flags[i].Key, flags[i].CreatedAt, flags[i-1].Key, flags[i-1].CreatedAt)
			}
			if !flags[i].UpdatedAt.Equal(flags[i].CreatedAt) {
				t.Errorf("%q updated_at %v, want it equal to created_at %v", flags[i].Key, flags[i].UpdatedAt, flags[i].CreatedAt)
			}
		}
	})
}

// Keys are stored trimmed and lowercased.
func TestCreateFlagNormalizesKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]struct{ key, want string }{
		"already normalized":  {"new-checkout", "new-checkout"},
		"capitalized":         {"New-Checkout", "new-checkout"},
		"all caps":            {"DARK-MODE", "dark-mode"},
		"digits kept":         {"V2-Api-2026", "v2-api-2026"},
		"digits only":         {"2026", "2026"},
		"single letter":       {"A", "a"},
		"surrounding spaces":  {"  dark-mode  ", "dark-mode"},
		"tabs and newlines":   {"\tdark-mode\n", "dark-mode"},
		"non-breaking spaces": {" dark-mode ", "dark-mode"},
		"100 characters":      {strings.Repeat("A", 100), strings.Repeat("a", 100)},
		"100 after trimming":  {"  " + strings.Repeat("a", 100) + "  ", strings.Repeat("a", 100)},
		"hyphens anywhere":    {"-a--b-", "-a--b-"},
		"hyphen only":         {"-", "-"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			got, err := store.CreateFlag(ctx, test.key)
			if err != nil {
				t.Fatalf("CreateFlag(%q): %v", test.key, err)
			}
			if got.Key != test.want {
				t.Errorf("CreateFlag(%q).Key = %q, want %q", test.key, got.Key, test.want)
			}
			if stored := storedFlag(t, store, got.ID).Key; stored != test.want {
				t.Errorf("stored key = %q, want %q", stored, test.want)
			}
		})
	}
}

func TestCreateFlagRejectsInvalidKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	invalid := func(msg string) *connect.Error {
		return connect.NewError(connect.CodeInvalidArgument, msg)
	}
	empty := invalid("can't create a flag with an empty key")
	whitespace := invalid("can't create a flag with whitespace in its key")
	tooLong := invalid("can't create a flag with a key over 100 characters")
	badChar := func(r string) *connect.Error {
		return invalid("can't create a flag with " + r + " in its key: only letters, digits and - are allowed")
	}

	tests := map[string]struct {
		key  string
		want *connect.Error
	}{
		"empty":                    {"", empty},
		"only spaces":              {"   ", empty},
		"only tabs and newlines":   {"\t\n\r", empty},
		"only non-breaking spaces": {"  ", empty},

		"inner space":                {"new checkout", whitespace},
		"inner tab":                  {"new\tcheckout", whitespace},
		"inner newline":              {"new\ncheckout", whitespace},
		"inner non-breaking space":   {"new checkout", whitespace},
		"inner space after trimming": {"  new checkout  ", whitespace},

		"underscore":     {"new_checkout", badChar("'_' (U+005F)")},
		"dot":            {"new.checkout", badChar("'.' (U+002E)")},
		"slash":          {"web/checkout", badChar("'/' (U+002F)")},
		"colon":          {"team:checkout", badChar("':' (U+003A)")},
		"accented":       {"école", badChar("'é' (U+00E9)")},
		"capital accent": {"École", badChar("'É' (U+00C9)")},
		"emoji":          {"rocket-🚀", badChar("'🚀' (U+1F680)")},
		// Lowercases to an ASCII "k", so it must be rejected before
		// lowercasing.
		"Kelvin sign": {"Key", badChar("'K' (U+212A)")},
		// Like the Kelvin sign, Go lowercases it to an ASCII "i".
		"dotted capital I": {"İd", badChar("'İ' (U+0130)")},

		"101 characters": {strings.Repeat("a", 101), tooLong},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			got, err := store.CreateFlag(ctx, test.key)
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("CreateFlag() = %+v on error, want the zero Flag", got)
			}
			if n := flagCount(t, store); n != 0 {
				t.Fatalf("%d flags after a rejected key, want 0", n)
			}
		})
	}
}

// Keys stay unique regardless of case and surrounding whitespace, since
// they are normalized before being stored.
func TestCreateFlagRejectsDuplicateKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exists := connect.NewError(connect.CodeAlreadyExists, "can't create the flag")

	for _, key := range []string{"new-checkout", "New-Checkout", "NEW-CHECKOUT", "  new-checkout  ", "\tNew-Checkout\n"} {
		t.Run(key, func(t *testing.T) {
			resetDB(t, store.db)
			first, err := store.CreateFlag(ctx, "new-checkout")
			if err != nil {
				t.Fatalf("CreateFlag() first call: %v", err)
			}
			got, err := store.CreateFlag(ctx, key)
			wantConnectErr(t, err, exists)
			if got != (database.Flag{}) {
				t.Fatalf("CreateFlag() = %+v on error, want the zero Flag", got)
			}
			if n := flagCount(t, store); n != 1 {
				t.Fatalf("%d flags after a duplicate, want 1", n)
			}
			if diff := cmp.Diff(first, storedFlag(t, store, first.ID)); diff != "" {
				t.Fatalf("first flag changed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)

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
			want: connect.NewError(connect.CodeCanceled, "can't create the flag"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't create the flag"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.CreateFlag(test.ctx, "new-checkout")
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("CreateFlag() = %+v on error, want the zero Flag", got)
			}
			if n := flagCount(t, store); n != 0 {
				t.Fatalf("%d flags after a failed create, want 0", n)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.CreateFlag(context.Background(), "new-checkout")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the flag"))
		if got != (database.Flag{}) {
			t.Fatalf("CreateFlag() = %+v on error, want the zero Flag", got)
		}
	})
}

// rejectEnvBumps makes moving an environment off version 1 fail until the test
// ends, leaving the rows already there alone.
func rejectEnvBumps(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec("ALTER TABLE environments ADD CONSTRAINT reject_bump CHECK (version = 1) NOT VALID"); err != nil {
		t.Fatalf("adding the constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec("ALTER TABLE environments DROP CONSTRAINT reject_bump"); err != nil {
			t.Errorf("dropping the constraint: %v", err)
		}
	})
}

// A new flag has a setting in every environment, so all of them change.
func TestCreateFlagBumpsEnvVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the version and updated_at move, the version by one from wherever
	// each environment was.
	t.Run("every environment goes up by one", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		before := []database.Environment{mustGetEnv(t, store, older.ID), newer}
		if before[0].Version == before[1].Version {
			t.Fatalf("both environments at version %d, want them to differ", before[0].Version)
		}
		mustCreateFlag(t, store, "new-checkout")
		for _, env := range before {
			got := mustGetEnv(t, store, env.ID)
			if !got.UpdatedAt.After(env.UpdatedAt) {
				t.Errorf("%s updated_at = %v, want it after %v", env.Name, got.UpdatedAt, env.UpdatedAt)
			}
			want := env
			want.Version++
			want.UpdatedAt = got.UpdatedAt
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", env.Name, diff)
			}
		}
	})

	t.Run("each new flag counts", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		for i := range 3 {
			mustCreateFlag(t, store, fmt.Sprintf("flag-%d", i))
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+3; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Flags racing to be created don't overwrite each other's bump.
	t.Run("racing flags all count", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.CreateFlag(ctx, fmt.Sprintf("flag-%d", i))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("CreateFlag(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	t.Run("no environments to bump", func(t *testing.T) {
		resetDB(t, store.db)
		if _, err := store.CreateFlag(ctx, "new-checkout"); err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("rejected flag leaves versions alone", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		for _, key := range []string{"", "not valid", "new-checkout"} {
			if _, err := store.CreateFlag(ctx, key); err == nil {
				t.Fatalf("CreateFlag(%q) = nil error, want it rejected", key)
			}
			if got := mustGetEnv(t, store, env.ID).Version; got != env.Version {
				t.Fatalf("version after CreateFlag(%q) = %d, want %d", key, got, env.Version)
			}
		}
	})

	t.Run("failed settings leave versions alone", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		rejectNewFlagSettings(t, store)
		if _, err := store.CreateFlag(ctx, "new-checkout"); err == nil {
			t.Fatal("CreateFlag() = nil error, want it to fail")
		}
		if got := mustGetEnv(t, store, env.ID).Version; got != env.Version {
			t.Fatalf("version = %d, want %d", got, env.Version)
		}
	})

	// The flag, its settings and the bump land together or not at all.
	t.Run("failed bump undoes the flag and its settings", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		got, err := store.CreateFlag(ctx, "new-checkout")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if n := flagCount(t, store); n != 0 {
			t.Errorf("%d flags after a failed bump, want 0", n)
		}
		if n := flagSettingCount(t, store); n != 0 {
			t.Errorf("%d flag settings after a failed bump, want 0", n)
		}
		if got := mustGetEnv(t, store, env.ID).Version; got != env.Version {
			t.Errorf("version = %d, want %d", got, env.Version)
		}
	})
}

// changeLogCount gives the number of committed change log entries.
func changeLogCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM change_logs").Scan(&n); err != nil {
		t.Fatalf("counting change logs: %v", err)
	}
	return n
}

// storedChangeLogs reads the change log entries for the flag key straight from
// the table, oldest version first.
func storedChangeLogs(t *testing.T, store *Store, key string) []database.ChangeLog {
	t.Helper()
	return queryChangeLogs(t, store, "flag_new_key = $1", key)
}

// storedChangeLogsFor reads the change log entries with the given action
// straight from the table, oldest version first.
func storedChangeLogsFor(t *testing.T, store *Store, action string) []database.ChangeLog {
	t.Helper()
	return queryChangeLogs(t, store, "action = $1", action)
}

// queryChangeLogs reads the change log entries matching where, oldest version
// first.
func queryChangeLogs(t *testing.T, store *Store, where string, args ...any) []database.ChangeLog {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT id, target_version, environment_id, flag_old_key, flag_new_key, enabled,
		  fallthrough, rule_serve, rule_clauses, rule_old_position, rule_new_position,
		  action, created_at, flag_id, flag_salt, rule_id
		FROM change_logs WHERE `+where+`
		ORDER BY target_version, environment_id`, args...)
	if err != nil {
		t.Fatalf("reading change logs: %v", err)
	}
	defer rows.Close()
	var logs []database.ChangeLog
	for rows.Next() {
		var l database.ChangeLog
		err := rows.Scan(&l.ID, &l.TargetVersion, &l.EnvironmentID, &l.FlagOldKey, &l.FlagNewKey, &l.Enabled,
			&l.Fallthrough, &l.RuleServe, &l.RuleClauses, &l.RuleOldPosition, &l.RuleNewPosition,
			&l.Action, &l.CreatedAt, &l.FlagID, &l.FlagSalt, &l.RuleID)
		if err != nil {
			t.Fatalf("reading change log: %v", err)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading change logs: %v", err)
	}
	return logs
}

// rejectNewChangeLogs makes logging a change fail until the test ends.
func rejectNewChangeLogs(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec("ALTER TABLE change_logs ADD CONSTRAINT reject_new CHECK (false) NOT VALID"); err != nil {
		t.Fatalf("adding the constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec("ALTER TABLE change_logs DROP CONSTRAINT reject_new"); err != nil {
			t.Errorf("dropping the constraint: %v", err)
		}
	})
}

// A new flag is logged in every environment, so SDKs can pick it up.
func TestCreateFlagLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Each entry carries the flag's normalized key, its id and salt, the setting
	// it starts with and the version its environment moved to, and nothing else.
	t.Run("one entry per environment", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		f, err := store.CreateFlag(ctx, " New-Checkout ")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		var want []database.ChangeLog
		for _, env := range []database.Environment{newer, older} {
			want = append(want, database.ChangeLog{
				TargetVersion: mustGetEnv(t, store, env.ID).Version,
				EnvironmentID: env.ID,
				FlagID:        &f.ID,
				FlagSalt:      sql.NullString{String: f.Salt, Valid: true},
				FlagNewKey:    sql.NullString{String: "new-checkout", Valid: true},
				Enabled:       sql.NullBool{Bool: false, Valid: true},
				Fallthrough:   sql.NullFloat64{Float64: 0, Valid: true},
				Action:        "flag-created",
			})
		}
		got := storedChangeLogs(t, store, f.Key)
		if want[0].TargetVersion > want[1].TargetVersion {
			want[0], want[1] = want[1], want[0]
		}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, got, ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got[0].ID == got[1].ID {
			t.Errorf("both entries have ID %v, want them to differ", got[0].ID)
		}
	})

	// The entries come from one change, so they share a time.
	t.Run("entries share a created_at", func(t *testing.T) {
		resetDB(t, store.db)
		for _, name := range []string{"staging", "production", "development"} {
			mustCreateEnv(t, store, name)
		}
		f := mustCreateFlag(t, store, "new-checkout")
		logs := storedChangeLogs(t, store, f.Key)
		if len(logs) != 3 {
			t.Fatalf("%d entries, want 3", len(logs))
		}
		for _, l := range logs[1:] {
			if !l.CreatedAt.Equal(logs[0].CreatedAt) {
				t.Errorf("created_at %v, want %v", l.CreatedAt, logs[0].CreatedAt)
			}
		}
	})

	// The entry is about the new flag only: the others are not logged again.
	t.Run("other flags are not logged again", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		mustCreateFlag(t, store, "dark-mode")
		before := changeLogCount(t, store)
		mustCreateFlag(t, store, "new-checkout")
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	t.Run("no environments to log", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "new-checkout")
		if n := changeLogCount(t, store); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// Flags racing to be created each log the version they moved to, so no
	// two entries claim the same one.
	t.Run("racing flags log their own versions", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.CreateFlag(ctx, fmt.Sprintf("flag-%d", i))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("CreateFlag(): %v", err)
			}
		}
		seen := map[int64]string{}
		for i := range racers {
			key := fmt.Sprintf("flag-%d", i)
			logs := storedChangeLogs(t, store, key)
			if len(logs) != 1 {
				t.Fatalf("%s has %d entries, want 1", key, len(logs))
			}
			v := logs[0].TargetVersion
			if v <= env.Version || v > env.Version+racers {
				t.Errorf("%s logged version %d, want it in (%d, %d]", key, v, env.Version, env.Version+racers)
			}
			if other, ok := seen[v]; ok {
				t.Errorf("%s and %s both logged version %d", other, key, v)
			}
			seen[v] = key
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("rejected flag logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		mustCreateFlag(t, store, "new-checkout")
		before := changeLogCount(t, store)
		for _, key := range []string{"", "not valid", "new-checkout"} {
			if _, err := store.CreateFlag(ctx, key); err == nil {
				t.Fatalf("CreateFlag(%q) = nil error, want it rejected", key)
			}
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries after CreateFlag(%q), want %d", got, key, before)
			}
		}
	})

	t.Run("failed settings log nothing", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		rejectNewFlagSettings(t, store)
		if _, err := store.CreateFlag(ctx, "new-checkout"); err == nil {
			t.Fatal("CreateFlag() = nil error, want it to fail")
		}
		if n := changeLogCount(t, store); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		if _, err := store.CreateFlag(ctx, "new-checkout"); err == nil {
			t.Fatal("CreateFlag() = nil error, want it to fail")
		}
		if n := changeLogCount(t, store); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The flag, its settings, the bump and the log land together or not at
	// all, so SDKs never miss a flag that exists.
	t.Run("failed log undoes the flag", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		rejectNewChangeLogs(t, store)
		got, err := store.CreateFlag(ctx, "new-checkout")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if n := flagCount(t, store); n != 0 {
			t.Errorf("%d flags after a failed log, want 0", n)
		}
		if n := flagSettingCount(t, store); n != 0 {
			t.Errorf("%d flag settings after a failed log, want 0", n)
		}
		if got := mustGetEnv(t, store, env.ID).Version; got != env.Version {
			t.Errorf("version = %d, want %d", got, env.Version)
		}
	})
}

func TestGetFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Several flags, so a lookup that ignores the ID and returns any row is
	// caught.
	created := make(map[string]database.Flag)
	for _, key := range []string{"new-checkout", "dark-mode", "v2-api"} {
		f, err := store.CreateFlag(ctx, key)
		if err != nil {
			t.Fatalf("CreateFlag(%q): %v", key, err)
		}
		created[key] = f
	}

	for key, want := range created {
		t.Run("finds "+key+" by its ID", func(t *testing.T) {
			got, err := store.GetFlag(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetFlag(%v): %v", want.ID, err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("GetFlag(%v) mismatch (-want +got):\n%s", want.ID, diff)
			}
		})
	}

	// The key comes back as it was stored, trimmed and lowercased.
	t.Run("returns the normalized key", func(t *testing.T) {
		f, err := store.CreateFlag(ctx, "  Beta-Banner  ")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		got, err := store.GetFlag(ctx, f.ID)
		if err != nil {
			t.Fatalf("GetFlag(%v): %v", f.ID, err)
		}
		if got.Key != "beta-banner" {
			t.Fatalf("GetFlag(%v).Key = %q, want %q", f.ID, got.Key, "beta-banner")
		}
	})

	// A new flag's timestamps are equal, so this catches created_at and
	// updated_at being read into the wrong fields.
	t.Run("returns distinct created_at and updated_at", func(t *testing.T) {
		f, err := store.CreateFlag(ctx, "aged")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		if _, err := store.db.Exec("UPDATE flags SET created_at = created_at - interval '1 day' WHERE id = $1", f.ID); err != nil {
			t.Fatalf("backdating created_at: %v", err)
		}
		want := storedFlag(t, store, f.ID)
		got, err := store.GetFlag(ctx, f.ID)
		if err != nil {
			t.Fatalf("GetFlag(%v): %v", f.ID, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("GetFlag(%v) mismatch (-stored +got):\n%s", f.ID, diff)
		}
	})

	notFound := connect.NewError(connect.CodeNotFound, "can't get the flag")
	t.Run("unknown ID is not found", func(t *testing.T) {
		got, err := store.GetFlag(ctx, uuid.New())
		wantConnectErr(t, err, notFound)
		if got != (database.Flag{}) {
			t.Fatalf("GetFlag() = %+v on error, want the zero Flag", got)
		}
	})
	t.Run("zero ID is not found", func(t *testing.T) {
		_, err := store.GetFlag(ctx, uuid.Nil())
		wantConnectErr(t, err, notFound)
	})
	// Flags and environments live in separate tables, so an environment's
	// ID never finds a flag.
	t.Run("environment ID is not found", func(t *testing.T) {
		env, err := store.CreateEnv(ctx, "production")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		_, err = store.GetFlag(ctx, env.ID)
		wantConnectErr(t, err, notFound)
	})
	t.Run("deleted flag is not found", func(t *testing.T) {
		f, err := store.CreateFlag(ctx, "temporary")
		if err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		resetDB(t, store.db)
		_, err = store.GetFlag(ctx, f.ID)
		wantConnectErr(t, err, notFound)
	})
}

func TestGetFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)
	f, err := store.CreateFlag(context.Background(), "new-checkout")
	if err != nil {
		t.Fatalf("CreateFlag(): %v", err)
	}

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
			want: connect.NewError(connect.CodeCanceled, "can't get the flag"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't get the flag"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.GetFlag(test.ctx, f.ID)
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("GetFlag() = %+v on error, want the zero Flag", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.GetFlag(context.Background(), f.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't get the flag"))
		if got != (database.Flag{}) {
			t.Fatalf("GetFlag() = %+v on error, want the zero Flag", got)
		}
	})
}

func mustCreateFlag(t *testing.T, store *Store, key string) database.Flag {
	t.Helper()
	f, err := store.CreateFlag(context.Background(), key)
	if err != nil {
		t.Fatalf("CreateFlag(%q): %v", key, err)
	}
	return f
}

func TestUpdateFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The key and updated_at change; the ID and created_at stay, and the
	// change is saved, not just returned.
	t.Run("changes the key", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		got, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
		if err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if got.ID != f.ID || got.Key != "checkout-v2" || !got.CreatedAt.Equal(f.CreatedAt) {
			t.Errorf("UpdateFlag() = %+v, want ID %v, key %q and created_at %v", got, f.ID, "checkout-v2", f.CreatedAt)
		}
		// A new salt would reshuffle the flag's users.
		if got.Salt != f.Salt {
			t.Errorf("UpdateFlag() salt = %q, want it kept as %q", got.Salt, f.Salt)
		}
		if !got.UpdatedAt.After(f.UpdatedAt) {
			t.Errorf("UpdateFlag() updated_at = %v, want it after %v", got.UpdatedAt, f.UpdatedAt)
		}
		if diff := cmp.Diff(got, storedFlag(t, store, f.ID)); diff != "" {
			t.Fatalf("stored flag mismatch (-returned +stored):\n%s", diff)
		}
	})

	t.Run("leaves other flags alone", func(t *testing.T) {
		resetDB(t, store.db)
		a := mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		c := mustCreateFlag(t, store, "c")
		if _, err := store.UpdateFlag(ctx, b.ID, "bb"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		for _, f := range []database.Flag{a, c} {
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("%s changed (-want +got):\n%s", f.Key, diff)
			}
		}
	})

	// The clock is read per statement, not per transaction, so each update
	// in one transaction gets a later time.
	t.Run("updates in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "a")
		var updates []database.Flag
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, key := range []string{"b", "c", "d"} {
				u, err := q.UpdateFlag(ctx, database.UpdateFlagParams{ID: f.ID, Key: key})
				if err != nil {
					return err
				}
				updates = append(updates, u)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		for i := 1; i < len(updates); i++ {
			if !updates[i].UpdatedAt.After(updates[i-1].UpdatedAt) {
				t.Errorf("%q updated_at %v not after %q updated_at %v", updates[i].Key, updates[i].UpdatedAt, updates[i-1].Key, updates[i-1].UpdatedAt)
			}
		}
	})

	// Same normalization as CreateFlag, in what is returned and what is
	// stored.
	t.Run("normalizes the new key", func(t *testing.T) {
		tests := map[string]struct{ key, want string }{
			"capitalized":        {"Checkout-V2", "checkout-v2"},
			"all caps":           {"CHECKOUT-V2", "checkout-v2"},
			"surrounding spaces": {"  checkout-v2  ", "checkout-v2"},
			"tabs and newlines":  {"\tcheckout-v2\n", "checkout-v2"},
			"100 characters":     {strings.Repeat("A", 100), strings.Repeat("a", 100)},
			"100 after trimming": {"  " + strings.Repeat("a", 100) + "  ", strings.Repeat("a", 100)},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				resetDB(t, store.db)
				f := mustCreateFlag(t, store, "new-checkout")
				got, err := store.UpdateFlag(ctx, f.ID, test.key)
				if err != nil {
					t.Fatalf("UpdateFlag(%q): %v", test.key, err)
				}
				if got.Key != test.want {
					t.Errorf("UpdateFlag(%q).Key = %q, want %q", test.key, got.Key, test.want)
				}
				if stored := storedFlag(t, store, f.ID).Key; stored != test.want {
					t.Errorf("stored key = %q, want %q", stored, test.want)
				}
			})
		}
	})

	// Updating to its own key, in any case or spacing, is not a conflict
	// with itself. It isn't an update either, so updated_at stays.
	t.Run("own key changes nothing", func(t *testing.T) {
		for _, key := range []string{"new-checkout", "NEW-CHECKOUT", " New-Checkout "} {
			t.Run(key, func(t *testing.T) {
				resetDB(t, store.db)
				f := mustCreateFlag(t, store, "new-checkout")
				got, err := store.UpdateFlag(ctx, f.ID, key)
				if err != nil {
					t.Fatalf("UpdateFlag(%q): %v", key, err)
				}
				if diff := cmp.Diff(f, got); diff != "" {
					t.Errorf("UpdateFlag(%q) mismatch (-want +got):\n%s", key, diff)
				}
				if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
					t.Errorf("flag changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Once a flag's key changes, its old key is free again.
	t.Run("old key can be reused", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v1"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		other := mustCreateFlag(t, store, "dark-mode")
		if _, err := store.UpdateFlag(ctx, other.ID, "checkout"); err != nil {
			t.Fatalf("UpdateFlag() to the freed key: %v", err)
		}
		mustCreateFlag(t, store, "dark-mode")
	})
}

func TestUpdateFlagRejectsInvalidKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	invalid := func(msg string) *connect.Error {
		return connect.NewError(connect.CodeInvalidArgument, msg)
	}
	empty := invalid("can't update a flag with an empty key")
	whitespace := invalid("can't update a flag with whitespace in its key")
	tooLong := invalid("can't update a flag with a key over 100 characters")
	badChar := func(r string) *connect.Error {
		return invalid("can't update a flag with " + r + " in its key: only letters, digits and - are allowed")
	}

	tests := map[string]struct {
		key  string
		want *connect.Error
	}{
		"empty":                    {"", empty},
		"only spaces":              {"   ", empty},
		"only non-breaking spaces": {"  ", empty},
		"inner space":              {"new checkout", whitespace},
		"inner tab":                {"new\tcheckout", whitespace},
		"underscore":               {"new_checkout", badChar("'_' (U+005F)")},
		"accented":                 {"école", badChar("'é' (U+00E9)")},
		"Kelvin sign":              {"Key", badChar("'K' (U+212A)")},
		"101 characters":           {strings.Repeat("a", 101), tooLong},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			f := mustCreateFlag(t, store, "new-checkout")
			got, err := store.UpdateFlag(ctx, f.ID, test.key)
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("UpdateFlag() = %+v on error, want the zero Flag", got)
			}
			// The rejected key was never written.
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("flag changed (-want +got):\n%s", diff)
			}
		})
	}

	// Validation comes first, so an invalid key is reported even for an ID
	// that doesn't exist.
	t.Run("invalid key checked before the ID", func(t *testing.T) {
		resetDB(t, store.db)
		_, err := store.UpdateFlag(ctx, uuid.New(), "   ")
		wantConnectErr(t, err, empty)
	})
}

// Keys stay unique regardless of case and surrounding whitespace when
// updating, as when creating.
func TestUpdateFlagRejectsDuplicateKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exists := connect.NewError(connect.CodeAlreadyExists, "can't update the flag")

	for _, key := range []string{"new-checkout", "New-Checkout", "NEW-CHECKOUT", "  new-checkout  ", "\tNew-Checkout\n"} {
		t.Run(key, func(t *testing.T) {
			resetDB(t, store.db)
			taken := mustCreateFlag(t, store, "new-checkout")
			f := mustCreateFlag(t, store, "dark-mode")
			got, err := store.UpdateFlag(ctx, f.ID, key)
			wantConnectErr(t, err, exists)
			if got != (database.Flag{}) {
				t.Fatalf("UpdateFlag() = %+v on error, want the zero Flag", got)
			}
			// Neither flag changed.
			for _, want := range []database.Flag{taken, f} {
				if diff := cmp.Diff(want, storedFlag(t, store, want.ID)); diff != "" {
					t.Fatalf("%s changed (-want +got):\n%s", want.Key, diff)
				}
			}
		})
	}
}

func TestUpdateFlagNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't update the flag")

	t.Run("unknown ID", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		got, err := store.UpdateFlag(ctx, uuid.New(), "checkout-v2")
		wantConnectErr(t, err, notFound)
		if got != (database.Flag{}) {
			t.Fatalf("UpdateFlag() = %+v on error, want the zero Flag", got)
		}
		// No other flag was updated instead.
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Fatalf("flag changed (-want +got):\n%s", diff)
		}
	})
	t.Run("zero ID", func(t *testing.T) {
		resetDB(t, store.db)
		_, err := store.UpdateFlag(ctx, uuid.Nil(), "checkout-v2")
		wantConnectErr(t, err, notFound)
	})
	t.Run("deleted flag", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		resetDB(t, store.db)
		_, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
		wantConnectErr(t, err, notFound)
	})
}

// An update made through ExecTx is undone when the transaction rolls back.
func TestUpdateFlagRolledBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	f := mustCreateFlag(t, store, "new-checkout")

	errBoom := errors.New("boom")
	err := store.ExecTx(ctx, func(q *database.Queries) error {
		if _, err := q.UpdateFlag(ctx, database.UpdateFlagParams{ID: f.ID, Key: "checkout-v2"}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
	}
	if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
		t.Fatalf("flag changed after rollback (-want +got):\n%s", diff)
	}
}

// Every environment serves a flag's key, so all of them change with it.
func TestUpdateFlagBumpsEnvVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the version and updated_at move, the version by one from wherever
	// each environment was.
	t.Run("every environment goes up by one", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		before := []database.Environment{mustGetEnv(t, store, older.ID), mustGetEnv(t, store, newer.ID)}
		if before[0].Version == before[1].Version {
			t.Fatalf("both environments at version %d, want them to differ", before[0].Version)
		}
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		for _, env := range before {
			got := mustGetEnv(t, store, env.ID)
			if !got.UpdatedAt.After(env.UpdatedAt) {
				t.Errorf("%s updated_at = %v, want it after %v", env.Name, got.UpdatedAt, env.UpdatedAt)
			}
			want := env
			want.Version++
			want.UpdatedAt = got.UpdatedAt
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", env.Name, diff)
			}
		}
	})

	t.Run("each change counts", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		for i := range 3 {
			if _, err := store.UpdateFlag(ctx, f.ID, fmt.Sprintf("key-%d", i)); err != nil {
				t.Fatalf("UpdateFlag(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+3; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("own key leaves versions alone", func(t *testing.T) {
		for _, key := range []string{"new-checkout", "NEW-CHECKOUT", " New-Checkout "} {
			t.Run(key, func(t *testing.T) {
				resetDB(t, store.db)
				f := mustCreateFlag(t, store, "new-checkout")
				env := mustCreateEnv(t, store, "production")
				if _, err := store.UpdateFlag(ctx, f.ID, key); err != nil {
					t.Fatalf("UpdateFlag(%q): %v", key, err)
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Going back to an earlier key is still a change from the current one.
	t.Run("returning to an old key counts", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		for _, key := range []string{"checkout-v2", "new-checkout"} {
			if _, err := store.UpdateFlag(ctx, f.ID, key); err != nil {
				t.Fatalf("UpdateFlag(%q): %v", key, err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+2; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Whichever gets there first makes the change; the others find the key
	// already set.
	t.Run("racing to the same key counts once", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlag(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Updates racing on one flag don't overwrite each other's bump.
	t.Run("racing updates all count", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateFlag(ctx, f.ID, fmt.Sprintf("key-%d", i))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlag(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	t.Run("no environments to bump", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("rejected update leaves versions alone", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "taken")
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		tests := map[string]struct {
			id   uuid.UUID
			key  string
			want connect.Code
		}{
			"invalid key": {f.ID, "not valid", connect.CodeInvalidArgument},
			"taken key":   {f.ID, "taken", connect.CodeAlreadyExists},
			"unknown ID":  {uuid.New(), "checkout-v2", connect.CodeNotFound},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				_, err := store.UpdateFlag(ctx, test.id, test.key)
				if got := connect.CodeOf(err); got != test.want {
					t.Fatalf("UpdateFlag() code = %v, want %v (error: %v)", got, test.want, err)
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// The new key and the bump land together or not at all.
	t.Run("failed bump keeps the old key", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		got, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Errorf("flag changed after a failed bump (-want +got):\n%s", diff)
		}
		if got := mustGetEnv(t, store, env.ID).Version; got != env.Version {
			t.Errorf("version = %d, want %d", got, env.Version)
		}
	})
}

// A new key is logged in every environment, so SDKs can follow the rename.
func TestUpdateFlagLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Each entry carries the key the flag had, its normalized new key and the
	// version its environment moved to, and nothing else.
	t.Run("one entry per environment", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, " Checkout-V2 "); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		var want []database.ChangeLog
		for _, env := range []database.Environment{older, newer} {
			want = append(want, database.ChangeLog{
				TargetVersion: mustGetEnv(t, store, env.ID).Version,
				EnvironmentID: env.ID,
				FlagOldKey:    sql.NullString{String: "new-checkout", Valid: true},
				FlagNewKey:    sql.NullString{String: "checkout-v2", Valid: true},
				Action:        "flag-updated",
			})
		}
		got := storedChangeLogsFor(t, store, "flag-updated")
		if want[0].TargetVersion > want[1].TargetVersion {
			want[0], want[1] = want[1], want[0]
		}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, got, ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got[0].ID == got[1].ID {
			t.Errorf("both entries have ID %v, want them to differ", got[0].ID)
		}
	})

	// The entries come from one change, so they share a time.
	t.Run("entries share a created_at", func(t *testing.T) {
		resetDB(t, store.db)
		for _, name := range []string{"staging", "production", "development"} {
			mustCreateEnv(t, store, name)
		}
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "flag-updated")
		if len(logs) != 3 {
			t.Fatalf("%d entries, want 3", len(logs))
		}
		for _, l := range logs[1:] {
			if !l.CreatedAt.Equal(logs[0].CreatedAt) {
				t.Errorf("created_at %v, want %v", l.CreatedAt, logs[0].CreatedAt)
			}
		}
	})

	// Only the renamed flag is logged.
	t.Run("other flags are not logged", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		mustCreateFlag(t, store, "dark-mode")
		f := mustCreateFlag(t, store, "new-checkout")
		before := changeLogCount(t, store)
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// Each rename names the key it replaced, so a flag renamed back and forth
	// can be followed step by step.
	t.Run("each rename names the key before it", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "a")
		keys := []string{"a", "b", "c", "a"}
		for _, key := range keys[1:] {
			if _, err := store.UpdateFlag(ctx, f.ID, key); err != nil {
				t.Fatalf("UpdateFlag(%q): %v", key, err)
			}
		}
		logs := storedChangeLogsFor(t, store, "flag-updated")
		if len(logs) != len(keys)-1 {
			t.Fatalf("%d entries, want %d", len(logs), len(keys)-1)
		}
		for i, l := range logs {
			if l.FlagOldKey.String != keys[i] || l.FlagNewKey.String != keys[i+1] {
				t.Errorf("entry %d renames %q to %q, want %q to %q", i, l.FlagOldKey.String, l.FlagNewKey.String, keys[i], keys[i+1])
			}
		}
	})

	// Renames racing on one flag each see the key the one before left, so the
	// entries chain without a gap.
	t.Run("racing renames chain", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "start")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateFlag(ctx, f.ID, fmt.Sprintf("key-%d", i))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlag(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "flag-updated")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		prev := "start"
		for _, l := range logs {
			if l.FlagOldKey.String != prev {
				t.Errorf("version %d renames from %q, want %q", l.TargetVersion, l.FlagOldKey.String, prev)
			}
			prev = l.FlagNewKey.String
		}
		if got := storedFlag(t, store, f.ID).Key; got != prev {
			t.Errorf("flag key = %q, want the last logged key %q", got, prev)
		}
	})

	t.Run("no environments to log", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if n := changeLogCount(t, store); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("own key logs nothing", func(t *testing.T) {
		for _, key := range []string{"new-checkout", "NEW-CHECKOUT", " New-Checkout "} {
			t.Run(key, func(t *testing.T) {
				resetDB(t, store.db)
				mustCreateEnv(t, store, "production")
				f := mustCreateFlag(t, store, "new-checkout")
				before := changeLogCount(t, store)
				if _, err := store.UpdateFlag(ctx, f.ID, key); err != nil {
					t.Fatalf("UpdateFlag(%q): %v", key, err)
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("rejected update logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		mustCreateFlag(t, store, "taken")
		f := mustCreateFlag(t, store, "new-checkout")
		before := changeLogCount(t, store)
		tests := map[string]struct {
			id  uuid.UUID
			key string
		}{
			"invalid key": {f.ID, "not valid"},
			"taken key":   {f.ID, "taken"},
			"unknown ID":  {uuid.New(), "checkout-v2"},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.UpdateFlag(ctx, test.id, test.key); err == nil {
					t.Fatalf("UpdateFlag() = nil error, want it rejected")
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err == nil {
			t.Fatal("UpdateFlag() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "flag-updated")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The new key, the bump and the log land together or not at all, so SDKs
	// never miss a rename.
	t.Run("failed log keeps the old key", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		before := mustGetEnv(t, store, env.ID)
		rejectNewChangeLogs(t, store)
		got, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Errorf("flag changed after a failed log (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(before, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

func TestUpdateFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)
	f := mustCreateFlag(t, store, "new-checkout")

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
			want: connect.NewError(connect.CodeCanceled, "can't update the flag"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't update the flag"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.UpdateFlag(test.ctx, f.ID, "checkout-v2")
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("UpdateFlag() = %+v on error, want the zero Flag", got)
			}
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("flag changed (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.UpdateFlag(context.Background(), f.ID, "checkout-v2")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag"))
		if got != (database.Flag{}) {
			t.Fatalf("UpdateFlag() = %+v on error, want the zero Flag", got)
		}
	})
}

func TestDeleteFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't delete the flag")
	// What the getters report once the flag and its settings are gone.
	flagGone := connect.NewError(connect.CodeNotFound, "can't get the flag")
	settingGone := connect.NewError(connect.CodeNotFound, "can't get the flag setting")

	t.Run("deletes and returns the flag as it was", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		got, err := store.DeleteFlag(ctx, f.ID)
		if err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if diff := cmp.Diff(f, got); diff != "" {
			t.Fatalf("DeleteFlag() mismatch (-want +got):\n%s", diff)
		}
		_, err = store.GetFlag(ctx, f.ID)
		wantConnectErr(t, err, flagGone)
	})

	// The returned flag is the last saved version, not the created one.
	t.Run("returns the flag after updates", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		updated, err := store.UpdateFlag(ctx, f.ID, "checkout-v2")
		if err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		got, err := store.DeleteFlag(ctx, f.ID)
		if err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if diff := cmp.Diff(updated, got); diff != "" {
			t.Fatalf("DeleteFlag() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("leaves other flags alone", func(t *testing.T) {
		resetDB(t, store.db)
		a := mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		c := mustCreateFlag(t, store, "c")
		if _, err := store.DeleteFlag(ctx, b.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if n := flagCount(t, store); n != 2 {
			t.Fatalf("%d flags after delete, want 2", n)
		}
		for _, f := range []database.Flag{a, c} {
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("%s changed (-want +got):\n%s", f.Key, diff)
			}
		}
	})

	// The flag's settings go with it, in every environment; other flags'
	// settings stay, and so do the environments, bumped.
	t.Run("deletes the flag's settings", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		other := mustCreateFlag(t, store, "dark-mode")
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		var kept []database.FlagSetting
		for _, e := range envs {
			kept = append(kept, mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: other.ID, EnvironmentID: e.ID, Fallthrough: 0.5}))
		}
		// Updating the settings bumped the environments.
		for i, e := range envs {
			envs[i] = mustGetEnv(t, store, e.ID)
		}
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		for _, e := range envs {
			_, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID})
			wantConnectErr(t, err, settingGone)
			got := mustGetEnv(t, store, e.ID)
			want := e
			want.Version++
			want.UpdatedAt = got.UpdatedAt
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("environment %s mismatch (-want +got):\n%s", e.Name, diff)
			}
		}
		if n := flagSettingCount(t, store); n != len(kept) {
			t.Fatalf("%d flag settings after delete, want %d", n, len(kept))
		}
		for _, s := range kept {
			if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
				t.Fatalf("other flag's setting changed (-want +got):\n%s", diff)
			}
		}
	})

	t.Run("deleting twice is not found", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag() first call: %v", err)
		}
		got, err := store.DeleteFlag(ctx, f.ID)
		wantConnectErr(t, err, notFound)
		if got != (database.Flag{}) {
			t.Fatalf("DeleteFlag() = %+v on error, want the zero Flag", got)
		}
	})

	t.Run("unknown ID deletes nothing", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		// An environment's ID never matches a flag.
		for _, id := range []uuid.UUID{uuid.New(), uuid.Nil(), env.ID} {
			got, err := store.DeleteFlag(ctx, id)
			wantConnectErr(t, err, notFound)
			if got != (database.Flag{}) {
				t.Fatalf("DeleteFlag(%v) = %+v on error, want the zero Flag", id, got)
			}
		}
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Fatalf("flag changed (-want +got):\n%s", diff)
		}
	})

	// A deleted flag's key is free again, and the new flag is a different
	// one with a new setting, not the old one's values.
	t.Run("key can be reused", func(t *testing.T) {
		resetDB(t, store.db)
		old := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		oldSetting := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: old.ID, EnvironmentID: env.ID, Enabled: true})
		if _, err := store.DeleteFlag(ctx, old.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		reborn := mustCreateFlag(t, store, "New-Checkout")
		if reborn.ID == old.ID {
			t.Fatalf("recreated flag reused ID %v", old.ID)
		}
		if reborn.Salt == old.Salt {
			t.Fatalf("recreated flag reused salt %q", old.Salt)
		}
		_, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: old.ID, EnvironmentID: env.ID})
		wantConnectErr(t, err, settingGone)
		got, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: reborn.ID, EnvironmentID: env.ID})
		if err != nil {
			t.Fatalf("GetFlagSetting(): %v", err)
		}
		if got.ID == oldSetting.ID || got.Enabled {
			t.Fatalf("recreated flag's setting = %+v, want a new, disabled one, not %+v", got, oldSetting)
		}
	})
}

// A delete made through ExecTx is undone when the transaction rolls back,
// settings included.
func TestDeleteFlagRolledBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	f := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: f.ID, EnvironmentID: env.ID, Fallthrough: 0.5})

	errBoom := errors.New("boom")
	err := store.ExecTx(ctx, func(q *database.Queries) error {
		if _, err := q.DeleteFlag(ctx, f.ID); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
	}
	if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
		t.Fatalf("flag changed after rollback (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
		t.Fatalf("setting changed after rollback (-want +got):\n%s", diff)
	}
}

// A deleted flag had a setting in every environment, so all of them change.
func TestDeleteFlagBumpsEnvVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the version and updated_at move, the version by one from wherever
	// each environment was.
	t.Run("every environment goes up by one", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		before := []database.Environment{mustGetEnv(t, store, older.ID), mustGetEnv(t, store, newer.ID)}
		if before[0].Version == before[1].Version {
			t.Fatalf("both environments at version %d, want them to differ", before[0].Version)
		}
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		for _, env := range before {
			got := mustGetEnv(t, store, env.ID)
			if !got.UpdatedAt.After(env.UpdatedAt) {
				t.Errorf("%s updated_at = %v, want it after %v", env.Name, got.UpdatedAt, env.UpdatedAt)
			}
			want := env
			want.Version++
			want.UpdatedAt = got.UpdatedAt
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", env.Name, diff)
			}
		}
	})

	// The second delete changes nothing, so readers have nothing to pick up.
	t.Run("not found leaves versions alone", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		want := mustGetEnv(t, store, env.ID)
		for _, id := range []uuid.UUID{f.ID, uuid.New(), uuid.Nil()} {
			_, err := store.DeleteFlag(ctx, id)
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't delete the flag"))
			if diff := cmp.Diff(want, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		}
	})

	// Flags racing to be deleted don't overwrite each other's bump.
	t.Run("racing deletes all count", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		var flags []database.Flag
		for i := range racers {
			flags = append(flags, mustCreateFlag(t, store, fmt.Sprintf("flag-%d", i)))
		}
		before := mustGetEnv(t, store, env.ID).Version
		errs := make(chan error, racers)
		for _, f := range flags {
			go func() {
				_, err := store.DeleteFlag(ctx, f.ID)
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("DeleteFlag(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, before+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	t.Run("no environments to bump", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
	})

	// The delete and the bump land together or not at all.
	t.Run("failed bump keeps the flag and its settings", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: f.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
		rejectEnvBumps(t, store)
		got, err := store.DeleteFlag(ctx, f.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Errorf("flag changed after a failed bump (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
			t.Errorf("setting changed after a failed bump (-want +got):\n%s", diff)
		}
	})
}

// A deleted flag is logged in every environment, so SDKs can drop it.
func TestDeleteFlagLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Each entry carries the key the flag had and the version its environment
	// moved to, and nothing else.
	t.Run("one entry per environment", func(t *testing.T) {
		resetDB(t, store.db)
		older := mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		newer := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		var want []database.ChangeLog
		for _, env := range []database.Environment{older, newer} {
			want = append(want, database.ChangeLog{
				TargetVersion: mustGetEnv(t, store, env.ID).Version,
				EnvironmentID: env.ID,
				FlagNewKey:    sql.NullString{String: "new-checkout", Valid: true},
				Action:        "flag-deleted",
			})
		}
		got := storedChangeLogsFor(t, store, "flag-deleted")
		if want[0].TargetVersion > want[1].TargetVersion {
			want[0], want[1] = want[1], want[0]
		}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, got, ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if len(got) == 2 && got[0].ID == got[1].ID {
			t.Errorf("both entries have ID %v, want them to differ", got[0].ID)
		}
	})

	// The entries come from one change, so they share a time.
	t.Run("entries share a created_at", func(t *testing.T) {
		resetDB(t, store.db)
		for _, name := range []string{"staging", "production", "development"} {
			mustCreateEnv(t, store, name)
		}
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "flag-deleted")
		if len(logs) != 3 {
			t.Fatalf("%d entries, want 3", len(logs))
		}
		for _, l := range logs[1:] {
			if !l.CreatedAt.Equal(logs[0].CreatedAt) {
				t.Errorf("created_at %v, want %v", l.CreatedAt, logs[0].CreatedAt)
			}
		}
	})

	// Only the deleted flag is logged.
	t.Run("other flags are not logged", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		mustCreateFlag(t, store, "dark-mode")
		f := mustCreateFlag(t, store, "new-checkout")
		before := changeLogCount(t, store)
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// The log is a record, so what was said about the flag stays after it's
	// gone.
	t.Run("earlier entries stay", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.UpdateFlag(ctx, f.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		before := changeLogCount(t, store)
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	t.Run("no environments to log", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if n := changeLogCount(t, store); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// Flags racing to be deleted each log the version they moved to, so no
	// two entries claim the same one.
	t.Run("racing deletes log their own versions", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		var flags []database.Flag
		for i := range racers {
			flags = append(flags, mustCreateFlag(t, store, fmt.Sprintf("flag-%d", i)))
		}
		before := mustGetEnv(t, store, env.ID).Version
		errs := make(chan error, racers)
		for _, f := range flags {
			go func() {
				_, err := store.DeleteFlag(ctx, f.ID)
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("DeleteFlag(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "flag-deleted")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		keys := map[string]bool{}
		for i, l := range logs {
			if want := before + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if keys[l.FlagNewKey.String] {
				t.Errorf("%s logged twice", l.FlagNewKey.String)
			}
			keys[l.FlagNewKey.String] = true
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("not found logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		if _, err := store.DeleteFlag(ctx, f.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		before := changeLogCount(t, store)
		for _, id := range []uuid.UUID{f.ID, uuid.New(), uuid.Nil()} {
			_, err := store.DeleteFlag(ctx, id)
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't delete the flag"))
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries after deleting %v, want %d", got, id, before)
			}
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		if _, err := store.DeleteFlag(ctx, f.ID); err == nil {
			t.Fatal("DeleteFlag() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "flag-deleted")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The delete, the bump and the log land together or not at all, so SDKs
	// never keep serving a flag that's gone.
	t.Run("failed log keeps the flag and its settings", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		f := mustCreateFlag(t, store, "new-checkout")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: f.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
		before := mustGetEnv(t, store, env.ID)
		rejectNewChangeLogs(t, store)
		got, err := store.DeleteFlag(ctx, f.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
			t.Errorf("flag changed after a failed log (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
			t.Errorf("setting changed after a failed log (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(before, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

func TestDeleteFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)
	f := mustCreateFlag(t, store, "new-checkout")

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
			want: connect.NewError(connect.CodeCanceled, "can't delete the flag"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't delete the flag"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.DeleteFlag(test.ctx, f.ID)
			wantConnectErr(t, err, test.want)
			if got != (database.Flag{}) {
				t.Fatalf("DeleteFlag() = %+v on error, want the zero Flag", got)
			}
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("flag changed (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.DeleteFlag(context.Background(), f.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the flag"))
		if got != (database.Flag{}) {
			t.Fatalf("DeleteFlag() = %+v on error, want the zero Flag", got)
		}
	})
}

func TestListFlags(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// list gives the first page, which is every flag in these cases, so
	// there must be no next page.
	list := func(t *testing.T) []database.Flag {
		t.Helper()
		got, next, err := store.ListFlags(ctx, nil)
		if err != nil {
			t.Fatalf("ListFlags(): %v", err)
		}
		if next != nil {
			t.Fatalf("ListFlags() next cursor = %+v with %d flags, want nil", *next, len(got))
		}
		return got
	}

	t.Run("no flags", func(t *testing.T) {
		resetDB(t, store.db)
		if got := list(t); len(got) != 0 {
			t.Fatalf("ListFlags() = %+v, want no flags", got)
		}
	})

	t.Run("returns every flag as stored", func(t *testing.T) {
		resetDB(t, store.db)
		a := mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		want := []database.Flag{storedFlag(t, store, a.ID), storedFlag(t, store, b.ID)}
		if diff := cmp.Diff(want, list(t)); diff != "" {
			t.Fatalf("ListFlags() mismatch (-want +got):\n%s", diff)
		}
	})

	// Keys only hold a-z, 0-9 and -, where the test database's collation
	// sorts as bytes do: - before digits before letters, and a key before the
	// longer keys it starts.
	t.Run("orders by key", func(t *testing.T) {
		resetDB(t, store.db)
		for _, key := range []string{"b", "a-c", "10", "a", "9", "a1", "ab", "a-b"} {
			mustCreateFlag(t, store, key)
		}
		want := []string{"10", "9", "a", "a-b", "a-c", "a1", "ab", "b"}
		if diff := cmp.Diff(want, keysOf(list(t))); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a renamed flag moves to its new key", func(t *testing.T) {
		resetDB(t, store.db)
		a := mustCreateFlag(t, store, "a")
		mustCreateFlag(t, store, "b")
		if _, err := store.UpdateFlag(ctx, a.ID, "c"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if diff := cmp.Diff([]string{"b", "c"}, keysOf(list(t))); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("leaves out deleted flags", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		mustCreateFlag(t, store, "c")
		if _, err := store.DeleteFlag(ctx, b.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if diff := cmp.Diff([]string{"a", "c"}, keysOf(list(t))); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
	})
}

// keysOf gives the keys of flags, in order.
func keysOf(flags []database.Flag) []string {
	keys := make([]string, len(flags))
	for i, f := range flags {
		keys[i] = f.Key
	}
	return keys
}

// mustCreateFlags creates n flags keyed flag-000, flag-001 and so on, which
// sort as they're numbered, and returns their keys in that order.
func mustCreateFlags(t *testing.T, store *Store, n int) []string {
	t.Helper()
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("flag-%03d", i)
		mustCreateFlag(t, store, keys[i])
	}
	return keys
}

// listPage lists the page after cursor, or the first page for a nil cursor,
// failing the test if the call fails.
func listPage(t *testing.T, store *Store, cursor *FlagsCursor) ([]database.Flag, *FlagsCursor) {
	t.Helper()
	flags, next, err := store.ListFlags(context.Background(), cursor)
	if err != nil {
		t.Fatalf("ListFlags(%+v): %v", cursor, err)
	}
	return flags, next
}

func TestListFlagsPages(t *testing.T) {
	store := newTestStore(t)

	t.Run("a full page is the last when nothing follows it", func(t *testing.T) {
		resetDB(t, store.db)
		keys := mustCreateFlags(t, store, MaxFlagsPerPage)
		got, next := listPage(t, store, nil)
		if diff := cmp.Diff(keys, keysOf(got)); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListFlags() next cursor = %+v, want nil", *next)
		}
	})

	// The first page stops at MaxFlagsPerPage flags, and its cursor is the
	// last key on it, so the next page starts with the flag after.
	t.Run("one flag past a page gives a second page", func(t *testing.T) {
		resetDB(t, store.db)
		keys := mustCreateFlags(t, store, MaxFlagsPerPage+1)

		first, next := listPage(t, store, nil)
		if diff := cmp.Diff(keys[:MaxFlagsPerPage], keysOf(first)); diff != "" {
			t.Fatalf("first page keys mismatch (-want +got):\n%s", diff)
		}
		want := FlagsCursor{Key: keys[MaxFlagsPerPage-1]}
		if next == nil || *next != want {
			t.Fatalf("first page next cursor = %v, want %+v", next, want)
		}

		second, next := listPage(t, store, next)
		if diff := cmp.Diff(keys[MaxFlagsPerPage:], keysOf(second)); diff != "" {
			t.Fatalf("second page keys mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("second page next cursor = %+v, want nil", *next)
		}
	})

	// Following the cursors lists every flag once, in key order.
	t.Run("following cursors lists every flag once", func(t *testing.T) {
		resetDB(t, store.db)
		keys := mustCreateFlags(t, store, 2*MaxFlagsPerPage+7)
		var got []string
		var cursor *FlagsCursor
		for pages := 1; ; pages++ {
			if pages > 4 {
				t.Fatalf("still listing after %d pages, at %+v", pages-1, cursor)
			}
			page, next := listPage(t, store, cursor)
			if len(page) > MaxFlagsPerPage {
				t.Fatalf("page %d has %d flags, want at most %d", pages, len(page), MaxFlagsPerPage)
			}
			got = append(got, keysOf(page)...)
			if next == nil {
				break
			}
			cursor = next
		}
		if diff := cmp.Diff(keys, got); diff != "" {
			t.Fatalf("listed keys mismatch (-want +got):\n%s", diff)
		}
	})

	// A zero cursor, as a decoded token can hold, has no key to start after.
	t.Run("a zero cursor starts at the first flag", func(t *testing.T) {
		resetDB(t, store.db)
		for _, key := range []string{"a", "b"} {
			mustCreateFlag(t, store, key)
		}
		got, next := listPage(t, store, &FlagsCursor{})
		if diff := cmp.Diff([]string{"a", "b"}, keysOf(got)); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListFlags() next cursor = %+v, want nil", *next)
		}
	})

	t.Run("starts after the cursor's key", func(t *testing.T) {
		resetDB(t, store.db)
		for _, key := range []string{"a", "b", "c"} {
			mustCreateFlag(t, store, key)
		}
		got, next := listPage(t, store, &FlagsCursor{Key: "a"})
		if diff := cmp.Diff([]string{"b", "c"}, keysOf(got)); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListFlags() next cursor = %+v, want nil", *next)
		}
	})

	// The cursor's flag can be renamed or deleted between pages; the next page
	// still starts at the first key after the cursor's.
	t.Run("a cursor whose flag is gone still works", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		mustCreateFlag(t, store, "c")
		if _, err := store.DeleteFlag(context.Background(), b.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		got, _ := listPage(t, store, &FlagsCursor{Key: "b"})
		if diff := cmp.Diff([]string{"c"}, keysOf(got)); diff != "" {
			t.Fatalf("ListFlags() keys mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a cursor past the last key gives an empty last page", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "a")
		got, next := listPage(t, store, &FlagsCursor{Key: "z"})
		if len(got) != 0 {
			t.Fatalf("ListFlags() = %+v, want no flags", got)
		}
		if next != nil {
			t.Fatalf("ListFlags() next cursor = %+v, want nil", *next)
		}
	})
}

func TestListFlagsReportsErrors(t *testing.T) {
	store := newTestStore(t)
	mustCreateFlag(t, store, "new-checkout")

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
			want: connect.NewError(connect.CodeCanceled, "can't list flags"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't list flags"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, next, err := store.ListFlags(test.ctx, nil)
			wantConnectErr(t, err, test.want)
			if got != nil || next != nil {
				t.Fatalf("ListFlags() = %+v, %v on error, want nil, nil", got, next)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, next, err := store.ListFlags(context.Background(), nil)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't list flags"))
		if got != nil || next != nil {
			t.Fatalf("ListFlags() = %+v, %v on error, want nil, nil", got, next)
		}
	})
}

// mustArchiveFlag archives the flag with id, failing the test if it fails.
func mustArchiveFlag(t *testing.T, store *Store, id uuid.UUID) {
	t.Helper()
	if err := store.ArchiveFlag(context.Background(), id); err != nil {
		t.Fatalf("ArchiveFlag(%v): %v", id, err)
	}
}

// archiveOps are ArchiveFlag and UnarchiveFlag, each with whether the flag it
// changes starts out archived, the action it logs and its error message.
var archiveOps = map[string]struct {
	call     func(*Store, context.Context, uuid.UUID) error
	archived bool
	action   string
	message  string
}{
	"ArchiveFlag":   {(*Store).ArchiveFlag, false, "flag-archived", "can't archive flag"},
	"UnarchiveFlag": {(*Store).UnarchiveFlag, true, "flag-unarchived", "can't unarchive flag"},
}

// mustCreateFlagFor creates a flag keyed key, archived when op starts from an
// archived flag.
func mustCreateFlagFor(t *testing.T, store *Store, archived bool, key string) database.Flag {
	t.Helper()
	f := mustCreateFlag(t, store, key)
	if archived {
		mustArchiveFlag(t, store, f.ID)
	}
	return storedFlag(t, store, f.ID)
}

// rejectArchivedChange makes moving a flag off archived = from fail until the
// test ends, leaving the rows already there alone.
func rejectArchivedChange(t *testing.T, store *Store, from bool) {
	t.Helper()
	if _, err := store.db.Exec(fmt.Sprintf("ALTER TABLE flags ADD CONSTRAINT reject_archive CHECK (archived = %t) NOT VALID", from)); err != nil {
		t.Fatalf("adding the constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec("ALTER TABLE flags DROP CONSTRAINT reject_archive"); err != nil {
			t.Errorf("dropping the constraint: %v", err)
		}
	})
}

func TestArchiveFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for name, op := range archiveOps {
		// Only archived moves: the key and salt evaluation hangs on stay.
		t.Run(name+" sets archived", func(t *testing.T) {
			resetDB(t, store.db)
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			want := f
			want.Archived = !op.archived
			ignore := cmpopts.IgnoreFields(database.Flag{}, "UpdatedAt")
			if diff := cmp.Diff(want, storedFlag(t, store, f.ID), ignore); diff != "" {
				t.Fatalf("flag mismatch (-want +got):\n%s", diff)
			}
		})

		t.Run(name+" leaves other flags alone", func(t *testing.T) {
			resetDB(t, store.db)
			other := mustCreateFlagFor(t, store, op.archived, "dark-mode")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			if diff := cmp.Diff(other, storedFlag(t, store, other.ID)); diff != "" {
				t.Fatalf("other flag changed (-want +got):\n%s", diff)
			}
		})

		// The flag's settings and rules are kept for when it's unarchived.
		t.Run(name+" leaves settings and rules alone", func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "production")
			f := mustCreateFlag(t, store, "new-checkout")
			s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: f.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
			r := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: s.ID, Name: "benin", Serve: 1, Clauses: json.RawMessage(validClauses)})
			if op.archived {
				mustArchiveFlag(t, store, f.ID)
			}
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
				t.Errorf("setting changed (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(r, storedRule(t, store, r.ID)); diff != "" {
				t.Errorf("rule changed (-want +got):\n%s", diff)
			}
		})

		t.Run(name+" not found", func(t *testing.T) {
			resetDB(t, store.db)
			gone := mustCreateFlag(t, store, "new-checkout")
			if _, err := store.DeleteFlag(ctx, gone.ID); err != nil {
				t.Fatalf("DeleteFlag(): %v", err)
			}
			for _, id := range []uuid.UUID{gone.ID, uuid.New(), uuid.Nil()} {
				err := op.call(store, ctx, id)
				wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, op.message))
			}
		})
	}

	for name, op := range archiveOps {
		// A flag already in the state asked for has nothing to change, so
		// readers have nothing to pick up.
		t.Run(name+" twice changes nothing the second time", func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, !op.archived, "new-checkout")
			envBefore := mustGetEnv(t, store, env.ID)
			logsBefore := changeLogCount(t, store)
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Errorf("flag changed (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(envBefore, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Errorf("environment changed (-want +got):\n%s", diff)
			}
			if got := changeLogCount(t, store); got != logsBefore {
				t.Errorf("%d entries, want %d", got, logsBefore)
			}
		})

		// The flag, the bump and the log land together or not at all.
		t.Run(name+" failed update bumps and logs nothing", func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			envBefore := mustGetEnv(t, store, env.ID)
			logsBefore := changeLogCount(t, store)
			rejectArchivedChange(t, store, op.archived)
			err := op.call(store, ctx, f.ID)
			wantConnectErr(t, err, connect.NewError(connect.CodeInternal, op.message))
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Errorf("flag changed (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(envBefore, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Errorf("environment changed (-want +got):\n%s", diff)
			}
			if got := changeLogCount(t, store); got != logsBefore {
				t.Errorf("%d entries, want %d", got, logsBefore)
			}
		})
	}

	// Archiving is undone by unarchiving, any number of times.
	t.Run("archive then unarchive", func(t *testing.T) {
		resetDB(t, store.db)
		f := mustCreateFlag(t, store, "new-checkout")
		for range 2 {
			mustArchiveFlag(t, store, f.ID)
			if !storedFlag(t, store, f.ID).Archived {
				t.Fatal("flag not archived after ArchiveFlag()")
			}
			if err := store.UnarchiveFlag(ctx, f.ID); err != nil {
				t.Fatalf("UnarchiveFlag(): %v", err)
			}
			if storedFlag(t, store, f.ID).Archived {
				t.Fatal("flag still archived after UnarchiveFlag()")
			}
		}
	})

	t.Run("a new flag isn't archived", func(t *testing.T) {
		resetDB(t, store.db)
		if f := mustCreateFlag(t, store, "new-checkout"); f.Archived {
			t.Fatalf("CreateFlag() = %+v, want it not archived", f)
		}
	})
}

// Archiving changes what every environment serves for the flag, so all of
// them move, as for any other change to the flag.
func TestArchiveFlagBumpsEnvVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for name, op := range archiveOps {
		t.Run(name+" moves every environment up by one", func(t *testing.T) {
			resetDB(t, store.db)
			older := mustCreateEnv(t, store, "staging")
			mustCreateFlag(t, store, "dark-mode")
			newer := mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			before := []database.Environment{mustGetEnv(t, store, older.ID), mustGetEnv(t, store, newer.ID)}
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			for _, env := range before {
				got := mustGetEnv(t, store, env.ID)
				want := env
				want.Version++
				want.UpdatedAt = got.UpdatedAt
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s mismatch (-want +got):\n%s", env.Name, diff)
				}
			}
		})

		t.Run(name+" not found leaves versions alone", func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "production")
			want := mustGetEnv(t, store, env.ID)
			if err := op.call(store, ctx, uuid.New()); err == nil {
				t.Fatalf("%s() = nil error for a missing flag", name)
			}
			if diff := cmp.Diff(want, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		})

		t.Run(name+" failed bump keeps the flag as it was", func(t *testing.T) {
			resetDB(t, store.db)
			mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			rejectEnvBumps(t, store)
			err := op.call(store, ctx, f.ID)
			wantConnectErr(t, err, connect.NewError(connect.CodeInternal, op.message))
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Fatalf("flag changed after a failed bump (-want +got):\n%s", diff)
			}
		})
	}
}

// Archiving and unarchiving are logged in every environment, so SDKs can stop
// or start serving the flag.
func TestArchiveFlagLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for name, op := range archiveOps {
		// Each entry carries the flag's key and the version its environment
		// moved to, and nothing else.
		t.Run(name+" one entry per environment", func(t *testing.T) {
			resetDB(t, store.db)
			older := mustCreateEnv(t, store, "staging")
			mustCreateFlag(t, store, "dark-mode")
			newer := mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			var want []database.ChangeLog
			for _, env := range []database.Environment{older, newer} {
				want = append(want, database.ChangeLog{
					TargetVersion: mustGetEnv(t, store, env.ID).Version,
					EnvironmentID: env.ID,
					FlagNewKey:    sql.NullString{String: "new-checkout", Valid: true},
					Action:        op.action,
				})
			}
			if want[0].TargetVersion > want[1].TargetVersion {
				want[0], want[1] = want[1], want[0]
			}
			got := storedChangeLogsFor(t, store, op.action)
			ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
			if diff := cmp.Diff(want, got, ignore); diff != "" {
				t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
			}
			if len(got) == 2 && got[0].ID == got[1].ID {
				t.Errorf("both entries have ID %v, want them to differ", got[0].ID)
			}
		})

		t.Run(name+" entries share a created_at", func(t *testing.T) {
			resetDB(t, store.db)
			for _, env := range []string{"staging", "production", "development"} {
				mustCreateEnv(t, store, env)
			}
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			logs := storedChangeLogsFor(t, store, op.action)
			if len(logs) != 3 {
				t.Fatalf("%d entries, want 3", len(logs))
			}
			for _, l := range logs[1:] {
				if !l.CreatedAt.Equal(logs[0].CreatedAt) {
					t.Errorf("created_at %v, want %v", l.CreatedAt, logs[0].CreatedAt)
				}
			}
		})

		t.Run(name+" no environments to log", func(t *testing.T) {
			resetDB(t, store.db)
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			if err := op.call(store, ctx, f.ID); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
			if n := len(storedChangeLogsFor(t, store, op.action)); n != 0 {
				t.Fatalf("%d entries, want 0", n)
			}
		})

		t.Run(name+" not found logs nothing", func(t *testing.T) {
			resetDB(t, store.db)
			mustCreateEnv(t, store, "production")
			before := changeLogCount(t, store)
			if err := op.call(store, ctx, uuid.New()); err == nil {
				t.Fatalf("%s() = nil error for a missing flag", name)
			}
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries, want %d", got, before)
			}
		})

		// The change, the bump and the log land together or not at all, so
		// SDKs never miss an archived flag.
		t.Run(name+" failed log keeps the flag and versions", func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "production")
			f := mustCreateFlagFor(t, store, op.archived, "new-checkout")
			before := mustGetEnv(t, store, env.ID)
			rejectNewChangeLogs(t, store)
			err := op.call(store, ctx, f.ID)
			wantConnectErr(t, err, connect.NewError(connect.CodeInternal, op.message))
			if diff := cmp.Diff(f, storedFlag(t, store, f.ID)); diff != "" {
				t.Errorf("flag changed after a failed log (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(before, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
			}
		})
	}
}

func TestArchiveFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)
	f := mustCreateFlag(t, store, "new-checkout")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for name, op := range archiveOps {
		for ctxName, test := range map[string]struct {
			ctx  context.Context
			code connect.Code
		}{
			"canceled context": {canceled, connect.CodeCanceled},
			"expired deadline": {expired, connect.CodeDeadlineExceeded},
		} {
			t.Run(name+" "+ctxName, func(t *testing.T) {
				before := storedFlag(t, store, f.ID)
				err := op.call(store, test.ctx, f.ID)
				wantConnectErr(t, err, connect.NewError(test.code, op.message))
				if diff := cmp.Diff(before, storedFlag(t, store, f.ID)); diff != "" {
					t.Fatalf("flag changed (-want +got):\n%s", diff)
				}
			})
		}
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		for _, op := range archiveOps {
			err := op.call(store, context.Background(), f.ID)
			wantConnectErr(t, err, connect.NewError(connect.CodeInternal, op.message))
		}
	})
}
