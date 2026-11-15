// Package leader makes background jobs leader-safe across replicas with
// Postgres advisory locks: a job runs on whichever replica takes its lock,
// the others skip that round.
package leader

import (
	"context"
	"database/sql"
	"fmt"
)

type LockerImpl interface {
	// Run calls fn while holding the lock for name. ran is false (and fn
	// isn't called) when another replica holds it.
	Run(ctx context.Context, name string, fn func(ctx context.Context) error) (ran bool, err error)
}

type PGLocker struct{ db *sql.DB }

func NewPGLocker(db *sql.DB) *PGLocker { return &PGLocker{db: db} }

func (l *PGLocker) Run(ctx context.Context, name string, fn func(ctx context.Context) error) (bool, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	key := "files.worker." + name
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", key).Scan(&got); err != nil {
		return false, fmt.Errorf("leader: lock %s: %w", name, err)
	}
	if !got {
		return false, nil
	}
	defer func() {
		// Unlock on a fresh context: ctx may be cancelled (shutdown).
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", key)
	}()
	return true, fn(ctx)
}

// Local runs every job (tests, single process).
type Local struct{}

func (Local) Run(ctx context.Context, _ string, fn func(ctx context.Context) error) (bool, error) {
	return true, fn(ctx)
}
