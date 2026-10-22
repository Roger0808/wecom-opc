package storage

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"weknora-tokendance/migrations"

	"gorm.io/gorm"
)

// migrationsFS 内嵌全部版本化迁移 SQL，启动时按序执行（见 RunMigrations）。
var migrationsFS = migrations.FS

// RunMigrations 在平台自有状态库连接上，单事务按版本号顺序执行未应用过的迁移，
// 并把版本记录到 schema_migrations 表。已记录的版本直接跳过，保证启动幂等；
// 任一迁移失败则回滚该版本并返回错误。
func (s *Storage) RunMigrations(ctx context.Context) error {
	if s.stateDB == nil {
		return fmt.Errorf("state db not configured")
	}
	db := s.stateDB.WithContext(ctx)
	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(64) PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`).Error; err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := migrationsFS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".up.sql")
		var count int64
		if err := db.Table("schema_migrations").
			Where("version = ?", version).Count(&count).Error; err != nil {
			return fmt.Errorf("check migration version %s: %w", version, err)
		}
		if count > 0 {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		err = db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(string(sqlBytes)).Error; err != nil {
				return fmt.Errorf("apply migration %s: %w", version, err)
			}
			return tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", version).Error
		})
		if err != nil {
			return err
		}
		log.Printf("[Storage] migration applied: %s", version)
	}
	return nil
}
