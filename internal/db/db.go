// Package db owns the GORM connection and transaction plumbing.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/boson-chat/files/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// DB wraps the GORM handle.
type DB struct {
	DB *gorm.DB
}

// NewDatabase opens a pooled Postgres connection.
func NewDatabase(ctx context.Context, cfg config.DBConfig) (*DB, error) {
	g, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger:                 gormlogger.Discard,
		SkipDefaultTransaction: true,
		NowFunc:                func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(max(cfg.MaxConns, 2))
	sqlDB.SetMaxIdleConns(max(cfg.MaxConns/2, 1))
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{DB: g}, nil
}

// SQL returns the underlying *sql.DB.
func (d *DB) SQL() (*sql.DB, error) { return d.DB.DB() }

// Close closes the pool.
func (d *DB) Close() error {
	s, err := d.DB.DB()
	if err != nil {
		return err
	}
	return s.Close()
}

type txKey struct{}

// Conn returns the transaction in ctx if there is one, else the pool, bound to ctx.
func (d *DB) Conn(ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txKey{}).(*gorm.DB); ok {
		return tx.WithContext(ctx)
	}
	return d.DB.WithContext(ctx)
}

// TxManagerImpl runs a function in a database transaction. Repositories
// called with the ctx passed to fn take part in it. Services use this to
// make multi-repository changes atomic without touching GORM.
type TxManagerImpl interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// WithTx implements TxManagerImpl. Nested calls reuse the outer transaction.
func (d *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*gorm.DB); ok {
		return fn(ctx)
	}
	return d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// NoTx is a TxManagerImpl for unit tests with in-memory repositories.
type NoTx struct{}

func (NoTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }
