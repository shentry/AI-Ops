package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type executionLease struct {
	conn *sql.Conn
	name string
}

// CheckExecutionLease must use the original pinned connection. A lost connection
// is a lost authority, even if ordinary pool connections reconnect successfully.
func (db *DB) CheckExecutionLease(ctx context.Context) error {
	if db.executionLease == nil {
		return errors.New("store: execution lock was not acquired")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var held sql.NullBool
	err := db.executionLease.conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?) = CONNECTION_ID()", db.executionLease.name).Scan(&held)
	if err != nil || !held.Valid || !held.Bool {
		return errors.New("store: execution lock connection lost; restart required")
	}
	return nil
}
