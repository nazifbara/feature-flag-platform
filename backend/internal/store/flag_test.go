package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/nazifbara/flagsvc/backend/internal/database"
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
	err := store.db.QueryRow("SELECT id, key, salt, created_at, updated_at FROM flags WHERE id = $1", id).
		Scan(&f.ID, &f.Key, &f.Salt, &f.CreatedAt, &f.UpdatedAt)
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
	rows, err := store.db.Query(`
		SELECT id, target_version, environment_id, flag_old_key, flag_new_key, enabled,
		  fallthrough, rule_serve, rule_clauses, rule_old_position, rule_new_position,
		  action, created_at
		FROM change_logs WHERE flag_new_key = $1
		ORDER BY target_version, environment_id`, key)
	if err != nil {
		t.Fatalf("reading change logs: %v", err)
	}
	defer rows.Close()
	var logs []database.ChangeLog
	for rows.Next() {
		var l database.ChangeLog
		err := rows.Scan(&l.ID, &l.TargetVersion, &l.EnvironmentID, &l.FlagOldKey, &l.FlagNewKey, &l.Enabled,
			&l.Fallthrough, &l.RuleServe, &l.RuleClauses, &l.RuleOldPosition, &l.RuleNewPosition,
			&l.Action, &l.CreatedAt)
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

	// Each entry carries the flag's normalized key, the setting it starts with
	// and the version its environment moved to, and nothing else.
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
