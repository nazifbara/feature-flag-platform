// Package store for database operations
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nazifbara/flagsvc/backend/internal/database"
)

type Store struct {
	*database.Queries
	db *sql.DB
}

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

func New(dbURL string) (*Store, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, err
	}
	return &Store{Queries: database.New(db), db: db}, nil
}
