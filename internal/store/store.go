package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// DB owns the application's GORM connection. Schema changes are applied by
// the checked-in SQL migrations, not by AutoMigrate.
type DB struct {
	*gorm.DB
}

// Open creates a MySQL-backed GORM database handle.
func Open(dsn string) (*DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("store: mysql DSN is required")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("store: open MySQL: %w", err)
	}
	return &DB{DB: db}, nil
}

// Close releases the underlying database connection pool.
func (db *DB) Close() error {
	if db == nil || db.DB == nil {
		return nil
	}

	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("store: access SQL database: %w", err)
	}
	return sqlDB.Close()
}
