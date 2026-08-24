package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 连接边界与跨域小工具。整包只有这里 import gorm 的 driver ——
// 其余文件按域切分，共用同一个 *DB 和同一套事务语义。

// DB 持有进程级 GORM 连接。D01 复用这一条；后续包不要自己再开连接池。
type DB struct {
	*gorm.DB
}

// Open 只建连。表结构走 migrations/001_init.sql，绝不 AutoMigrate。
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

// Close 关闭底层连接池。
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

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

// normalizePageLimit 把调用方传来的 limit 夹到 [1, maxPageLimit]，
// 0 视为"没指定"走默认值。所有游标分页查询共用这一处上界。
func normalizePageLimit(limit int) int {
	if limit < 1 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// truncateStoreText 是落库前的统一截断：先去首尾空白，超长按 rune 截并加省略号。
// 审计文本（event summary / problem detail）都过这一道，避免撑爆列宽。
func truncateStoreText(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
