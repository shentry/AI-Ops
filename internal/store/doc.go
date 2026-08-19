// Package store 是 D01 的数据库边界。
//
// 只有本包可以 import gorm.io/gorm。表结构在 migrations/001_init.sql，
// 手工执行；Open 不做 AutoMigrate。
package store
