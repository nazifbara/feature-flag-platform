// Package store for database operations
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nazifbara/flagsvc/admin-api/internal/database"
)

type Store struct {
	*database.Queries
	db *sql.DB
}

// ExecTx runs fn in a transaction, committing if it returns nil and rolling back
// if it fails or panics. Errors aren't mapped to connect errors here.
func (s *Store) ExecTx(ctx context.Context, fn func(*database.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	qtx := s.WithTx(tx)
	err = fn(qtx)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("tx error: %w, rollback error: %w", err, rbErr)
		}
		return err
	}
	return tx.Commit()
}

// New returns a Store for the database at dbURL. It doesn't connect yet, so a bad
// address only shows up on the first query.
func New(dbURL string) (*Store, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, err
	}
	return &Store{Queries: database.New(db), db: db}, nil
}
