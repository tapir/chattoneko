package config

import (
	"context"
	"database/sql"
)

// TestStore returns a Store backed by an already-migrated database and seeded
// with cfg (bypassing the first-run defaults). It exists so tests in other
// packages can construct arbitrary configurations, including incomplete ones.
// finalize() still runs, so empty values pick up their defaults; validate()
// runs too, so an auth-enabled config must carry a non-empty plaintext
// password. Unlike NewStore, auth here comes from the cfg argument rather than
// the environment.
func TestStore(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	s := &Store{db: db}
	if _, err := s.forceSet(ctx, cfg); err != nil {
		return nil, err
	}
	return s, nil
}
