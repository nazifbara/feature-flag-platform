package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/testutil"
)

// newTestStore connects to TEST_DB_URL and starts from empty tables.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DB_URL")
	if dbURL == "" {
		t.Fatal("TEST_DB_URL not set")
	}
	store, err := New(dbURL)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	t.Cleanup(func() { store.db.Close() })
	testutil.ResetDB(t, store.db)
	return store
}

// wantConnectErr fails the test unless err is a *connect.Error with want's
// code and message.
func wantConnectErr(t *testing.T, err error, want *connect.Error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want error = %q, code = %d", want.Error(), want.Code())
	}
	got, ok := err.(*connect.Error)
	if !ok {
		t.Fatalf("error = %v (%T), want *connect.Error", err, err)
	}
	if got.Code() != want.Code() || got.Error() != want.Error() {
		t.Fatalf("error, code = %v, %d, want error, code = %v, %d", got.Error(), got.Code(), want.Error(), want.Code())
	}
}

func TestEnvironmentCreation(t *testing.T) {
	store := newTestStore(t)

	empty := connect.NewError(connect.CodeInvalidArgument, "can't create an environment with an empty name")
	tooLong := connect.NewError(connect.CodeInvalidArgument, "can't create an environment with a name over 100 characters")
	tests := map[string]struct {
		name string
		// want is the stored name, when it differs from name.
		want string
		err  *connect.Error
	}{
		"reject empty name": {
			name: "",
			err:  empty,
		},
		// A name with only whitespace is as empty as "".
		"reject a single space": {
			name: " ",
			err:  empty,
		},
		"reject only spaces": {
			name: "    ",
			err:  empty,
		},
		"reject tabs and newlines": {
			name: "\t\n\r ",
			err:  empty,
		},
		"reject only non-breaking spaces": {
			name: "  ",
			err:  empty,
		},
		"accept a one character name": {
			name: "a",
		},
		"accept a name with inner spaces": {
			name: "staging eu",
		},
		// The column is VARCHAR(100): 100 characters fit, 101 must be
		// rejected before they reach the database.
		"accept a 100 character name": {
			name: strings.Repeat("a", 100),
		},
		"reject name over 100 characters": {
			name: strings.Repeat("a", 101),
			err:  tooLong,
		},
		// The limit counts characters, not bytes: "é" is 2 bytes.
		"accept 100 multibyte characters": {
			name: strings.Repeat("é", 100),
		},
		"reject 101 multibyte characters": {
			name: strings.Repeat("é", 101),
			err:  tooLong,
		},
		// Surrounding whitespace is trimmed before the limit is checked.
		"accept 100 characters with surrounding whitespace": {
			name: "  " + strings.Repeat("a", 100) + "  ",
			want: strings.Repeat("a", 100),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			got, err := store.CreateEnv(context.Background(), test.name)
			if test.err != nil {
				wantConnectErr(t, err, test.err)
				return
			}
			if err != nil {
				t.Fatalf("CreateEnv() unexpected error: %v", err)
			}
			wantName := test.want
			if wantName == "" {
				wantName = test.name
			}
			if got.Name != wantName {
				t.Fatalf("CreateEnv().Name = %q, want %q", got.Name, wantName)
			}
			// The ID is random, so it is only checked for being set.
			if got.ID == (database.Environment{}).ID {
				t.Fatal("CreateEnv() returned a zero ID")
			}
		})
	}
}

// Names are stored trimmed and lowercased, both in what CreateEnv returns
// and in the row it writes.
// A new environment has never been updated.
func TestEnvironmentCreationSetsUpdatedAt(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "production")
	if !env.UpdatedAt.Equal(env.CreatedAt) {
		t.Fatalf("CreateEnv() updated_at = %v, want it equal to created_at %v", env.UpdatedAt, env.CreatedAt)
	}
}

func TestEnvironmentCreationNormalizesName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]struct{ name, want string }{
		"capitalized":          {"Prod", "prod"},
		"all caps":             {"PRODUCTION", "production"},
		"mixed case":           {"sTaGiNg", "staging"},
		"non-ASCII letters":    {"ÉCOLE", "école"},
		"dotted capital I":     {"İzmir", "izmir"},
		"surrounding spaces":   {"  prod  ", "prod"},
		"tabs and newlines":    {"\tprod\n", "prod"},
		"spaces and case":      {" Staging EU ", "staging eu"},
		"inner spaces kept":    {"staging  eu", "staging  eu"},
		"already normalized":   {"prod", "prod"},
		"100 uppercase letter": {strings.Repeat("A", 100), strings.Repeat("a", 100)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			got, err := store.CreateEnv(ctx, test.name)
			if err != nil {
				t.Fatalf("CreateEnv(%q): %v", test.name, err)
			}
			if got.Name != test.want {
				t.Errorf("CreateEnv(%q).Name = %q, want %q", test.name, got.Name, test.want)
			}
			var stored string
			if err := store.db.QueryRowContext(ctx, "SELECT name FROM environments WHERE id = $1", got.ID).Scan(&stored); err != nil {
				t.Fatalf("reading back environment %v: %v", got.ID, err)
			}
			if stored != test.want {
				t.Errorf("stored name = %q, want %q", stored, test.want)
			}
		})
	}
}

// Names are unique regardless of case: "Prod" and "prod" are the same
// environment.
func TestEnvironmentCreationRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exists := connect.NewError(connect.CodeAlreadyExists, "can't create the environment")

	tests := map[string]struct{ first, second string }{
		"same name":           {"production", "production"},
		"capitalized":         {"prod", "Prod"},
		"lowercased":          {"Prod", "prod"},
		"all caps":            {"production", "PRODUCTION"},
		"mixed case":          {"staging", "sTaGiNg"},
		"non-ASCII letters":   {"école", "ÉCOLE"},
		"inner spaces differ": {"staging eu", "Staging EU"},
		"surrounding spaces":  {"prod", "  prod  "},
		"tabs and newlines":   {"prod", "\tprod\n"},
		"spaces and case":     {"prod", " PROD "},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			if _, err := store.CreateEnv(ctx, test.first); err != nil {
				t.Fatalf("CreateEnv(%q): %v", test.first, err)
			}
			_, err := store.CreateEnv(ctx, test.second)
			wantConnectErr(t, err, exists)
		})
	}

	// A different name is still accepted, with its own ID.
	t.Run("different name accepted", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		first, err := store.CreateEnv(ctx, "production")
		if err != nil {
			t.Fatalf("CreateEnv() first call: %v", err)
		}
		other, err := store.CreateEnv(ctx, "staging")
		if err != nil {
			t.Fatalf("CreateEnv() different name: %v", err)
		}
		if other.ID == first.ID {
			t.Fatalf("CreateEnv() reused ID %v", first.ID)
		}
	})
}

func TestGetEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Several environments, so a lookup that ignores the ID and returns
	// any row is caught.
	created := make(map[string]database.Environment)
	for _, name := range []string{"development", "staging", "production"} {
		env, err := store.CreateEnv(ctx, name)
		if err != nil {
			t.Fatalf("CreateEnv(%q): %v", name, err)
		}
		created[name] = env
	}

	for name, want := range created {
		t.Run("finds "+name+" by its ID", func(t *testing.T) {
			got, err := store.GetEnv(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetEnv(%v): %v", want.ID, err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("GetEnv(%v) mismatch (-want +got):\n%s", want.ID, diff)
			}
		})
	}

	// The name comes back as it was stored, trimmed and lowercased.
	t.Run("returns the normalized name", func(t *testing.T) {
		env, err := store.CreateEnv(ctx, "  QA  ")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		got, err := store.GetEnv(ctx, env.ID)
		if err != nil {
			t.Fatalf("GetEnv(%v): %v", env.ID, err)
		}
		if got.Name != "qa" {
			t.Fatalf("GetEnv(%v).Name = %q, want %q", env.ID, got.Name, "qa")
		}
	})

	notFound := connect.NewError(connect.CodeNotFound, "can't get the environment")
	t.Run("unknown ID is not found", func(t *testing.T) {
		got, err := store.GetEnv(ctx, uuid.New())
		wantConnectErr(t, err, notFound)
		if got != (database.Environment{}) {
			t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
		}
	})
	t.Run("zero ID is not found", func(t *testing.T) {
		_, err := store.GetEnv(ctx, uuid.Nil())
		wantConnectErr(t, err, notFound)
	})
	t.Run("deleted environment is not found", func(t *testing.T) {
		env, err := store.CreateEnv(ctx, "temporary")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		testutil.ResetDB(t, store.db)
		_, err = store.GetEnv(ctx, env.ID)
		wantConnectErr(t, err, notFound)
	})
}

// Failures that are neither a missing row nor a Postgres error still
// surface as errors with a matching code, never as an empty environment.
func TestGetEnvReportsNonDBErrors(t *testing.T) {
	store := newTestStore(t)
	env, err := store.CreateEnv(context.Background(), "production")
	if err != nil {
		t.Fatalf("CreateEnv(): %v", err)
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
			want: connect.NewError(connect.CodeCanceled, "can't get the environment"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't get the environment"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.GetEnv(test.ctx, env.ID)
			wantConnectErr(t, err, test.want)
			if got != (database.Environment{}) {
				t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.GetEnv(context.Background(), env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't get the environment"))
		if got != (database.Environment{}) {
			t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
		}
	})
}

func TestListEnvs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("no environments", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListEnvs() = %+v, want no environments", got)
		}
	})

	t.Run("one environment", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env, err := store.CreateEnv(ctx, "production")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		if diff := cmp.Diff([]database.Environment{env}, got); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})

	// Oldest first. The names are not in alphabetical order, so sorting by
	// name instead would fail.
	t.Run("every environment in creation order", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		var want []database.Environment
		for _, name := range []string{"staging", "production", "development", "qa", "demo"} {
			env, err := store.CreateEnv(ctx, name)
			if err != nil {
				t.Fatalf("CreateEnv(%q): %v", name, err)
			}
			want = append(want, env)
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})

	// Each insert gets its own creation time, so environments created in
	// one transaction keep their order too.
	t.Run("creation order within a transaction", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		var want []database.Environment
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, name := range []string{"staging", "production", "development", "qa", "demo"} {
				env, err := q.CreateEnv(ctx, name)
				if err != nil {
					return err
				}
				want = append(want, env)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})

	// Rows usually come back in insertion order even without an ORDER BY,
	// so the creation times are rewritten to differ from it: only sorting
	// by created_at gives the expected order.
	t.Run("sorted by created_at, not insertion order", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		created := map[string]time.Time{
			"production":  base.Add(2 * time.Hour),
			"staging":     base,
			"development": base.Add(time.Hour),
		}
		for _, name := range []string{"production", "staging", "development"} {
			env, err := store.CreateEnv(ctx, name)
			if err != nil {
				t.Fatalf("CreateEnv(%q): %v", name, err)
			}
			if _, err := store.db.ExecContext(ctx, "UPDATE environments SET created_at = $1 WHERE id = $2", created[name], env.ID); err != nil {
				t.Fatalf("setting created_at of %q: %v", name, err)
			}
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		var names []string
		for _, env := range got {
			names = append(names, env.Name)
		}
		if diff := cmp.Diff([]string{"staging", "development", "production"}, names); diff != "" {
			t.Fatalf("ListEnvs() order mismatch (-want +got):\n%s", diff)
		}
	})

	// Names come back as stored, trimmed and lowercased.
	t.Run("returns normalized names", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		for _, name := range []string{"  Production ", "STAGING"} {
			if _, err := store.CreateEnv(ctx, name); err != nil {
				t.Fatalf("CreateEnv(%q): %v", name, err)
			}
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		var names []string
		for _, env := range got {
			names = append(names, env.Name)
		}
		if diff := cmp.Diff([]string{"production", "staging"}, names, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
	})

	// Rejected creations, and writes rolled back in a transaction, never
	// show up.
	t.Run("only committed environments", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env, err := store.CreateEnv(ctx, "production")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		if _, err := store.CreateEnv(ctx, "PRODUCTION"); err == nil {
			t.Fatal("CreateEnv() duplicate = nil error, want AlreadyExists")
		}
		if _, err := store.CreateEnv(ctx, "   "); err == nil {
			t.Fatal("CreateEnv() blank = nil error, want InvalidArgument")
		}
		errBoom := errors.New("boom")
		err = store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.CreateEnv(ctx, "staging"); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
		}

		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		if diff := cmp.Diff([]database.Environment{env}, got); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})
}

// envNamesOf gives the names of envs, in order.
func envNamesOf(envs []database.Environment) []string {
	names := make([]string, len(envs))
	for i, e := range envs {
		names[i] = e.Name
	}
	return names
}

// mustCreateEnvs creates n environments named env-000, env-001 and so on, in
// that order, and returns their names in it.
func mustCreateEnvs(t *testing.T, store *Store, n int) []string {
	t.Helper()
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("env-%03d", i)
		mustCreateEnv(t, store, names[i])
	}
	return names
}

// listEnvsPage lists the page after cursor, or the first page for a nil
// cursor, failing the test if the call fails.
func listEnvsPage(t *testing.T, store *Store, cursor *EnvsCursor) ([]database.Environment, *EnvsCursor) {
	t.Helper()
	envs, next, err := store.ListEnvs(context.Background(), cursor)
	if err != nil {
		t.Fatalf("ListEnvs(%+v): %v", cursor, err)
	}
	return envs, next
}

// cursorAt is the cursor a page ending at env gives.
func cursorAt(env database.Environment) *EnvsCursor {
	return &EnvsCursor{CreatedAt: env.CreatedAt, ID: env.ID}
}

func TestListEnvsPages(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("a full page is the last when nothing follows it", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		names := mustCreateEnvs(t, store, MaxEnvsPerPage)
		got, next := listEnvsPage(t, store, nil)
		if diff := cmp.Diff(names, envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListEnvs() next cursor = %+v, want nil", *next)
		}
	})

	// The first page stops at MaxEnvsPerPage environments, and its cursor is
	// the last one on it, so the next page starts with the one created after.
	t.Run("one environment past a page gives a second page", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		names := mustCreateEnvs(t, store, MaxEnvsPerPage+1)

		first, next := listEnvsPage(t, store, nil)
		if diff := cmp.Diff(names[:MaxEnvsPerPage], envNamesOf(first)); diff != "" {
			t.Fatalf("first page names mismatch (-want +got):\n%s", diff)
		}
		want := cursorAt(first[MaxEnvsPerPage-1])
		if next == nil || !next.CreatedAt.Equal(want.CreatedAt) || next.ID != want.ID {
			t.Fatalf("first page next cursor = %+v, want %+v", next, *want)
		}

		second, next := listEnvsPage(t, store, next)
		if diff := cmp.Diff(names[MaxEnvsPerPage:], envNamesOf(second)); diff != "" {
			t.Fatalf("second page names mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("second page next cursor = %+v, want nil", *next)
		}
	})

	// Following the cursors lists every environment once, oldest first.
	t.Run("following cursors lists every environment once", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		names := mustCreateEnvs(t, store, 2*MaxEnvsPerPage+7)
		var got []string
		var cursor *EnvsCursor
		for pages := 1; ; pages++ {
			if pages > 4 {
				t.Fatalf("still listing after %d pages, at %+v", pages-1, cursor)
			}
			page, next := listEnvsPage(t, store, cursor)
			if len(page) > MaxEnvsPerPage {
				t.Fatalf("page %d has %d environments, want at most %d", pages, len(page), MaxEnvsPerPage)
			}
			got = append(got, envNamesOf(page)...)
			if next == nil {
				break
			}
			cursor = next
		}
		if diff := cmp.Diff(names, got); diff != "" {
			t.Fatalf("listed names mismatch (-want +got):\n%s", diff)
		}
	})

	// A zero cursor, as a decoded token can hold, has nothing to start after.
	t.Run("a zero cursor starts at the oldest environment", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		names := mustCreateEnvs(t, store, 2)
		got, next := listEnvsPage(t, store, &EnvsCursor{})
		if diff := cmp.Diff(names, envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListEnvs() next cursor = %+v, want nil", *next)
		}
	})

	t.Run("starts after the cursor's environment", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		a := mustCreateEnv(t, store, "staging")
		mustCreateEnv(t, store, "production")
		mustCreateEnv(t, store, "development")
		got, next := listEnvsPage(t, store, cursorAt(a))
		if diff := cmp.Diff([]string{"production", "development"}, envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
		if next != nil {
			t.Fatalf("ListEnvs() next cursor = %+v, want nil", *next)
		}
	})

	// Environments created at the same instant are ordered by id, and a
	// cursor at the first of them still reaches the others.
	t.Run("ties on created_at go by id", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		var tied []database.Environment
		for _, name := range []string{"staging", "production", "development"} {
			tied = append(tied, mustCreateEnv(t, store, name))
		}
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := store.db.ExecContext(ctx, "UPDATE environments SET created_at = $1", at); err != nil {
			t.Fatalf("setting created_at: %v", err)
		}
		sort.Slice(tied, func(i, j int) bool { return tied[i].ID.String() < tied[j].ID.String() })
		var want []string
		for _, e := range tied {
			want = append(want, e.Name)
		}
		got, _ := listEnvsPage(t, store, nil)
		if diff := cmp.Diff(want, envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
		got, _ = listEnvsPage(t, store, &EnvsCursor{CreatedAt: at, ID: tied[0].ID})
		if diff := cmp.Diff(want[1:], envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() after %s names mismatch (-want +got):\n%s", want[0], diff)
		}
	})

	// The cursor's environment can be deleted between pages; the next page
	// still starts at the first one created after it.
	t.Run("a cursor whose environment is gone still works", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		mustCreateEnv(t, store, "staging")
		b := mustCreateEnv(t, store, "production")
		mustCreateEnv(t, store, "development")
		if _, err := store.DeleteEnv(ctx, b.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		got, _ := listEnvsPage(t, store, cursorAt(b))
		if diff := cmp.Diff([]string{"development"}, envNamesOf(got)); diff != "" {
			t.Fatalf("ListEnvs() names mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a cursor past the newest environment gives an empty last page", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		e := mustCreateEnv(t, store, "staging")
		got, next := listEnvsPage(t, store, &EnvsCursor{CreatedAt: e.CreatedAt.Add(time.Hour), ID: e.ID})
		if len(got) != 0 {
			t.Fatalf("ListEnvs() = %+v, want no environments", got)
		}
		if next != nil {
			t.Fatalf("ListEnvs() next cursor = %+v, want nil", *next)
		}
	})
}

func TestListEnvsReportsErrors(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.CreateEnv(context.Background(), "production"); err != nil {
		t.Fatalf("CreateEnv(): %v", err)
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
			want: connect.NewError(connect.CodeCanceled, "can't list environments"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't list environments"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, _, err := store.ListEnvs(test.ctx, nil)
			wantConnectErr(t, err, test.want)
			if got != nil {
				t.Fatalf("ListEnvs() = %+v on error, want nil", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, _, err := store.ListEnvs(context.Background(), nil)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't list environments"))
		if got != nil {
			t.Fatalf("ListEnvs() = %+v on error, want nil", got)
		}
	})
}

// mustCreateEnv creates an environment or fails the test.
func mustCreateEnv(t *testing.T, store *Store, name string) database.Environment {
	t.Helper()
	env, err := store.CreateEnv(context.Background(), name)
	if err != nil {
		t.Fatalf("CreateEnv(%q): %v", name, err)
	}
	return env
}

// mustGetEnv reads an environment back or fails the test.
func mustGetEnv(t *testing.T, store *Store, id uuid.UUID) database.Environment {
	t.Helper()
	env, err := store.GetEnv(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEnv(%v): %v", id, err)
	}
	return env
}

func TestUpdateEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the name and updated_at change: the ID and creation time stay, and
	// the change is saved, not just returned.
	t.Run("renames the environment", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		got, err := store.UpdateEnv(ctx, env.ID, "preprod")
		if err != nil {
			t.Fatalf("UpdateEnv(): %v", err)
		}
		if !got.UpdatedAt.After(env.UpdatedAt) {
			t.Errorf("UpdateEnv() updated_at = %v, want it after %v", got.UpdatedAt, env.UpdatedAt)
		}
		want := database.Environment{ID: env.ID, Name: "preprod", CreatedAt: env.CreatedAt, UpdatedAt: got.UpdatedAt, Version: env.Version}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("UpdateEnv() mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(want, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Fatalf("GetEnv() after UpdateEnv() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("leaves other environments alone", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		dev := mustCreateEnv(t, store, "development")
		staging := mustCreateEnv(t, store, "staging")
		prod := mustCreateEnv(t, store, "production")
		if _, err := store.UpdateEnv(ctx, staging.ID, "preprod"); err != nil {
			t.Fatalf("UpdateEnv(): %v", err)
		}
		for _, env := range []database.Environment{dev, prod} {
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("%s changed (-want +got):\n%s", env.Name, diff)
			}
		}
	})

	// created_at is kept, so a renamed environment keeps its place in the
	// list.
	t.Run("keeps its place in the list", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		mustCreateEnv(t, store, "development")
		staging := mustCreateEnv(t, store, "staging")
		mustCreateEnv(t, store, "production")
		if _, err := store.UpdateEnv(ctx, staging.ID, "aaa"); err != nil {
			t.Fatalf("UpdateEnv(): %v", err)
		}
		got, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		var names []string
		for _, env := range got {
			names = append(names, env.Name)
		}
		if diff := cmp.Diff([]string{"development", "aaa", "production"}, names); diff != "" {
			t.Fatalf("ListEnvs() order mismatch (-want +got):\n%s", diff)
		}
	})

	// Same normalization as CreateEnv, in what is returned and what is
	// stored.
	t.Run("normalizes the new name", func(t *testing.T) {
		tests := map[string]struct{ name, want string }{
			"capitalized":         {"Preprod", "preprod"},
			"all caps":            {"PREPROD", "preprod"},
			"non-ASCII letters":   {"ÉCOLE", "école"},
			"surrounding spaces":  {"  preprod  ", "preprod"},
			"tabs and newlines":   {"\tpreprod\n", "preprod"},
			"inner spaces kept":   {" Staging  EU ", "staging  eu"},
			"100 characters":      {strings.Repeat("A", 100), strings.Repeat("a", 100)},
			"100 after trimming":  {"  " + strings.Repeat("a", 100) + "  ", strings.Repeat("a", 100)},
			"100 multibyte chars": {strings.Repeat("É", 100), strings.Repeat("é", 100)},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, store.db)
				env := mustCreateEnv(t, store, "staging")
				got, err := store.UpdateEnv(ctx, env.ID, test.name)
				if err != nil {
					t.Fatalf("UpdateEnv(%q): %v", test.name, err)
				}
				if got.Name != test.want {
					t.Errorf("UpdateEnv(%q).Name = %q, want %q", test.name, got.Name, test.want)
				}
				if stored := mustGetEnv(t, store, env.ID).Name; stored != test.want {
					t.Errorf("stored name = %q, want %q", stored, test.want)
				}
			})
		}
	})

	// Renaming to its own name, in any case or spacing, is not a conflict
	// with itself.
	t.Run("own name accepted", func(t *testing.T) {
		for _, name := range []string{"production", "PRODUCTION", " Production "} {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, store.db)
				env := mustCreateEnv(t, store, "production")
				got, err := store.UpdateEnv(ctx, env.ID, name)
				if err != nil {
					t.Fatalf("UpdateEnv(%q): %v", name, err)
				}
				// Writing the same name again still counts as an update.
				if !got.UpdatedAt.After(env.UpdatedAt) {
					t.Errorf("UpdateEnv(%q) updated_at = %v, want it after %v", name, got.UpdatedAt, env.UpdatedAt)
				}
				want := env
				want.UpdatedAt = got.UpdatedAt
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("UpdateEnv(%q) mismatch (-want +got):\n%s", name, diff)
				}
			})
		}
	})

	// Once an environment is renamed, its old name is free again.
	t.Run("old name can be reused", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		prod := mustCreateEnv(t, store, "prod")
		if _, err := store.UpdateEnv(ctx, prod.ID, "production"); err != nil {
			t.Fatalf("UpdateEnv(): %v", err)
		}
		staging := mustCreateEnv(t, store, "staging")
		if _, err := store.UpdateEnv(ctx, staging.ID, "prod"); err != nil {
			t.Fatalf("UpdateEnv() to the freed name: %v", err)
		}
		mustCreateEnv(t, store, "staging")
	})
}

func TestUpdateEnvRejectsInvalidName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	empty := connect.NewError(connect.CodeInvalidArgument, "can't update an environment with an empty name")
	tooLong := connect.NewError(connect.CodeInvalidArgument, "can't update an environment with a name over 100 characters")

	tests := map[string]struct {
		name string
		want *connect.Error
	}{
		"empty name":               {"", empty},
		"single space":             {" ", empty},
		"tabs and newlines":        {"\t\n\r ", empty},
		"non-breaking spaces":      {"  ", empty},
		"101 characters":           {strings.Repeat("a", 101), tooLong},
		"101 multibyte characters": {strings.Repeat("é", 101), tooLong},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			env := mustCreateEnv(t, store, "staging")
			got, err := store.UpdateEnv(ctx, env.ID, test.name)
			wantConnectErr(t, err, test.want)
			if got != (database.Environment{}) {
				t.Fatalf("UpdateEnv() = %+v on error, want the zero Environment", got)
			}
			// The rejected name was never written.
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		})
	}

	// Validation comes first, so an invalid name is reported even for an ID
	// that doesn't exist.
	t.Run("invalid name checked before the ID", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		_, err := store.UpdateEnv(ctx, uuid.New(), "   ")
		wantConnectErr(t, err, empty)
	})
}

// Names stay unique regardless of case and surrounding whitespace when
// renaming, as when creating.
func TestUpdateEnvRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exists := connect.NewError(connect.CodeAlreadyExists, "can't update the environment")

	for _, name := range []string{"production", "Production", "PRODUCTION", "  production  ", "\tProduction\n"} {
		t.Run(name, func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			prod := mustCreateEnv(t, store, "production")
			staging := mustCreateEnv(t, store, "staging")
			got, err := store.UpdateEnv(ctx, staging.ID, name)
			wantConnectErr(t, err, exists)
			if got != (database.Environment{}) {
				t.Fatalf("UpdateEnv() = %+v on error, want the zero Environment", got)
			}
			// Neither environment changed.
			for _, env := range []database.Environment{prod, staging} {
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("%s changed (-want +got):\n%s", env.Name, diff)
				}
			}
		})
	}
}

func TestUpdateEnvNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't update the environment")

	t.Run("unknown ID", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		got, err := store.UpdateEnv(ctx, uuid.New(), "preprod")
		wantConnectErr(t, err, notFound)
		if got != (database.Environment{}) {
			t.Fatalf("UpdateEnv() = %+v on error, want the zero Environment", got)
		}
		// No other environment was renamed instead.
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Fatalf("environment changed (-want +got):\n%s", diff)
		}
	})
	t.Run("zero ID", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		_, err := store.UpdateEnv(ctx, uuid.Nil(), "preprod")
		wantConnectErr(t, err, notFound)
	})
	t.Run("deleted environment", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		testutil.ResetDB(t, store.db)
		_, err := store.UpdateEnv(ctx, env.ID, "preprod")
		wantConnectErr(t, err, notFound)
	})
}

// A rename made through ExecTx is undone when the transaction rolls back.
func TestUpdateEnvRolledBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	env := mustCreateEnv(t, store, "staging")

	errBoom := errors.New("boom")
	err := store.ExecTx(ctx, func(q *database.Queries) error {
		if _, err := q.UpdateEnv(ctx, database.UpdateEnvParams{ID: env.ID, Name: "preprod"}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
	}
	if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
		t.Fatalf("environment changed after rollback (-want +got):\n%s", diff)
	}
}

func TestUpdateEnvReportsErrors(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "staging")

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
			want: connect.NewError(connect.CodeCanceled, "can't update the environment"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't update the environment"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.UpdateEnv(test.ctx, env.ID, "preprod")
			wantConnectErr(t, err, test.want)
			if got != (database.Environment{}) {
				t.Fatalf("UpdateEnv() = %+v on error, want the zero Environment", got)
			}
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.UpdateEnv(context.Background(), env.ID, "preprod")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the environment"))
		if got != (database.Environment{}) {
			t.Fatalf("UpdateEnv() = %+v on error, want the zero Environment", got)
		}
	})
}

func TestDeleteEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't delete the environment")
	// What GetEnv reports once the environment is gone.
	envGone := connect.NewError(connect.CodeNotFound, "can't get the environment")

	// names gives the names ListEnvs returns, in order.
	names := func(t *testing.T) []string {
		t.Helper()
		envs, _, err := store.ListEnvs(ctx, nil)
		if err != nil {
			t.Fatalf("ListEnvs(): %v", err)
		}
		var out []string
		for _, env := range envs {
			out = append(out, env.Name)
		}
		return out
	}

	t.Run("deletes and returns the environment as it was", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		got, err := store.DeleteEnv(ctx, env.ID)
		if err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		if diff := cmp.Diff(env, got); diff != "" {
			t.Fatalf("DeleteEnv() mismatch (-want +got):\n%s", diff)
		}
		_, err = store.GetEnv(ctx, env.ID)
		wantConnectErr(t, err, envGone)
	})

	// Only the given environment goes, and the rest keep their order.
	t.Run("leaves other environments alone", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		dev := mustCreateEnv(t, store, "development")
		staging := mustCreateEnv(t, store, "staging")
		prod := mustCreateEnv(t, store, "production")
		if _, err := store.DeleteEnv(ctx, staging.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		for _, env := range []database.Environment{dev, prod} {
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("%s changed (-want +got):\n%s", env.Name, diff)
			}
		}
		if diff := cmp.Diff([]string{"development", "production"}, names(t)); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("deleting twice is not found", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		if _, err := store.DeleteEnv(ctx, env.ID); err != nil {
			t.Fatalf("DeleteEnv() first call: %v", err)
		}
		got, err := store.DeleteEnv(ctx, env.ID)
		wantConnectErr(t, err, notFound)
		if got != (database.Environment{}) {
			t.Fatalf("DeleteEnv() = %+v on error, want the zero Environment", got)
		}
	})

	t.Run("unknown ID deletes nothing", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		mustCreateEnv(t, store, "development")
		mustCreateEnv(t, store, "production")
		for _, id := range []uuid.UUID{uuid.New(), uuid.Nil()} {
			_, err := store.DeleteEnv(ctx, id)
			wantConnectErr(t, err, notFound)
		}
		if diff := cmp.Diff([]string{"development", "production"}, names(t)); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})

	// A deleted environment's name is free again, and the new environment
	// is a different one.
	t.Run("name can be reused", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		old := mustCreateEnv(t, store, "staging")
		if _, err := store.DeleteEnv(ctx, old.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		reborn := mustCreateEnv(t, store, "Staging")
		if reborn.ID == old.ID {
			t.Fatalf("recreated environment reused ID %v", old.ID)
		}
		_, err := store.GetEnv(ctx, old.ID)
		wantConnectErr(t, err, envGone)
	})

	// A recreated environment is new, so it goes to the end of the list.
	t.Run("recreated environment is listed last", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		staging := mustCreateEnv(t, store, "staging")
		mustCreateEnv(t, store, "production")
		if _, err := store.DeleteEnv(ctx, staging.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		mustCreateEnv(t, store, "staging")
		if diff := cmp.Diff([]string{"production", "staging"}, names(t)); diff != "" {
			t.Fatalf("ListEnvs() mismatch (-want +got):\n%s", diff)
		}
	})
}

// A delete made through ExecTx is undone when the transaction rolls back.
func TestDeleteEnvRolledBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	env := mustCreateEnv(t, store, "staging")

	errBoom := errors.New("boom")
	err := store.ExecTx(ctx, func(q *database.Queries) error {
		if _, err := q.DeleteEnv(ctx, env.ID); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
	}
	if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
		t.Fatalf("environment changed after rollback (-want +got):\n%s", diff)
	}
}

// A deleted environment is logged, so its SDKs learn it's gone.
func TestDeleteEnvLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The entry carries the environment and the version after the one it was
	// at, and nothing else. The other environment, the flags and their settings
	// and rules are around, so an entry for any of them would show.
	t.Run("one entry for the environment", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a")
		mustCreateEnv(t, store, "staging")
		mustCreateFlag(t, store, "dark-mode")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		before := changeLogCount(t, store)
		if _, err := store.DeleteEnv(ctx, env.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		want := []database.ChangeLog{{
			TargetVersion: env.Version + 1,
			EnvironmentID: env.ID,
			Action:        "environment-deleted",
		}}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, storedChangeLogsFor(t, store, "environment-deleted"), ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// The entries are a record, so what was said about the environment stays
	// after it's gone, and the deletion comes last, after everything else
	// logged there.
	t.Run("comes after the environment's earlier entries", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a", "b")
		mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: setting.EnvironmentID, Enabled: true})
		earlier := queryChangeLogs(t, store, "environment_id = $1", setting.EnvironmentID)
		if len(earlier) == 0 {
			t.Fatal("no earlier entries, want some")
		}
		if _, err := store.DeleteEnv(ctx, setting.EnvironmentID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		logs := queryChangeLogs(t, store, "environment_id = $1", setting.EnvironmentID)
		if len(logs) != len(earlier)+1 {
			t.Fatalf("%d entries, want %d", len(logs), len(earlier)+1)
		}
		if diff := cmp.Diff(earlier, logs[:len(earlier)]); diff != "" {
			t.Errorf("earlier entries changed (-want +got):\n%s", diff)
		}
		last := logs[len(logs)-1]
		if last.Action != "environment-deleted" {
			t.Fatalf("last entry is %q, want environment-deleted", last.Action)
		}
		if prev := earlier[len(earlier)-1].TargetVersion; last.TargetVersion != prev+1 {
			t.Errorf("logged version %d, want %d, one after the last entry", last.TargetVersion, prev+1)
		}
	})

	// A write racing the delete either gets in first, and is logged before
	// the deletion, or finds nothing, so the deletion is always the
	// environment's last entry.
	t.Run("racing writes are logged before it", func(t *testing.T) {
		for range 20 {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "a")
			writes := []func() error{
				func() error {
					_, err := store.UpdateFlagSetting(ctx, FlagSettingUpdate{ID: setting.ID, Enabled: new(true), Fallthrough: new(float32(0))})
					return err
				},
				func() error {
					_, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rules[0].ID, Name: "a", Serve: 0.75, Clauses: rules[0].Clauses}))
					return err
				},
				func() error {
					_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
					return err
				},
			}
			errs := make(chan error, len(writes))
			for _, w := range writes {
				go func() { errs <- w() }()
			}
			if _, err := store.DeleteEnv(ctx, setting.EnvironmentID); err != nil {
				t.Fatalf("DeleteEnv(): %v", err)
			}
			for range writes {
				// A write that comes second finds nothing to change.
				if err := <-errs; err != nil && connect.CodeOf(err) != connect.CodeNotFound {
					t.Errorf("write: %v", err)
				}
			}
			logs := queryChangeLogs(t, store, "environment_id = $1", setting.EnvironmentID)
			last := logs[len(logs)-1]
			if last.Action != "environment-deleted" {
				t.Fatalf("last entry is %q at version %d, want environment-deleted", last.Action, last.TargetVersion)
			}
			for i, l := range logs[1:] {
				if l.TargetVersion != logs[i].TargetVersion+1 {
					t.Fatalf("entry %d logged version %d after %d, want them consecutive", i+1, l.TargetVersion, logs[i].TargetVersion)
				}
			}
		}
	})

	// Only the first of several racing deletes finds the environment, so it
	// is logged once.
	t.Run("racing deletes log once", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.DeleteEnv(ctx, env.ID)
				errs <- err
			}()
		}
		for range racers {
			<-errs
		}
		if n := len(storedChangeLogsFor(t, store, "environment-deleted")); n != 1 {
			t.Fatalf("%d entries, want 1", n)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("not found logs nothing", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		if _, err := store.DeleteEnv(ctx, env.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		before := changeLogCount(t, store)
		for _, id := range []uuid.UUID{env.ID, uuid.New(), uuid.Nil()} {
			_, err := store.DeleteEnv(ctx, id)
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't delete the environment"))
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries after deleting %v, want %d", got, id, before)
			}
		}
	})

	// The delete and the log land together or not at all, so SDKs never keep
	// serving an environment that's gone.
	t.Run("failed log keeps the environment", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		want := matrixState(t, store)
		rejectNewChangeLogs(t, store)
		got, err := store.DeleteEnv(ctx, env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the environment"))
		if diff := cmp.Diff(database.Environment{}, got); diff != "" {
			t.Fatalf("returned an environment on error (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
		if got := matrixState(t, store); got != want {
			t.Errorf("state = %s, want %s", got, want)
		}
		if n := flagSettingCount(t, store); n != 1 {
			t.Errorf("%d flag settings after a failed log, want 1", n)
		}
	})
}

func TestDeleteEnvReportsErrors(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "staging")

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
			want: connect.NewError(connect.CodeCanceled, "can't delete the environment"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't delete the environment"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.DeleteEnv(test.ctx, env.ID)
			wantConnectErr(t, err, test.want)
			if got != (database.Environment{}) {
				t.Fatalf("DeleteEnv() = %+v on error, want the zero Environment", got)
			}
			// The failed delete left the environment in place.
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.DeleteEnv(context.Background(), env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the environment"))
		if got != (database.Environment{}) {
			t.Fatalf("DeleteEnv() = %+v on error, want the zero Environment", got)
		}
	})
}
