package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
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
	err := store.db.QueryRow("SELECT id, key, created_at, updated_at FROM flags WHERE id = $1", id).
		Scan(&f.ID, &f.Key, &f.CreatedAt, &f.UpdatedAt)
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
		if got.ID == uuid.Nil {
			t.Error("CreateFlag() returned a zero ID")
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

	// The clock is read per flag, not per transaction, so flags created
	// together still get increasing times.
	t.Run("flags created in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		var flags []database.Flag
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, key := range []string{"a", "b", "c"} {
				f, err := q.CreateFlag(ctx, key)
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
	exists := connect.NewError(connect.CodeAlreadyExists, connect.CodeAlreadyExists.String())

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
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
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
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
		if got != (database.Flag{}) {
			t.Fatalf("CreateFlag() = %+v on error, want the zero Flag", got)
		}
	})
}
