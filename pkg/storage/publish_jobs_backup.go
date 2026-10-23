// WORK-RESOURCES-001（TASK-013，ADR-008/DATA-001，BND-011）：skill_publish_jobs
// 删除前的受控备份与恢复工具。迁移 000017 drop 目标表之前必须完成导出并校验
// 可恢复；备份为纯行数据 + 校验和，不含任何秘密。恢复流程：down 重建空表 →
// ImportSkillPublishJobs 校验 checksum 后在事务内插回存量行。
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// skillPublishJobsBackupVersion 备份结构版本（随字段集演进递增）。
const skillPublishJobsBackupVersion = 1

// skillPublishJobsTable 目标账本表（迁移 000017 唯一清理对象）。
const skillPublishJobsTable = "skill_publish_jobs"

// ErrSkillPublishJobsBackupInvalid 表示备份结构/校验和不符，拒绝恢复。
var ErrSkillPublishJobsBackupInvalid = errors.New("skill publish jobs backup invalid")

// SkillPublishJobBackupRow 单条发布任务备份行（与 000011/000017 结构一致；
// targets/steps 保留原始 JSON 文本，保证逐字节往返）。
type SkillPublishJobBackupRow struct {
	JobID          string          `json:"job_id"`
	ReleaseID      string          `json:"release_id"`
	SourceTenant   string          `json:"source_tenant"`
	Targets        json.RawMessage `json:"targets"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         string          `json:"status"`
	Steps          json.RawMessage `json:"steps,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// SkillPublishJobsBackup 账本备份（行规范化 JSON + SHA-256 校验和）。
type SkillPublishJobsBackup struct {
	Version  int                        `json:"version"`
	Table    string                     `json:"table"`
	RowCount int                        `json:"row_count"`
	SHA256   string                     `json:"sha256"`
	Rows     []SkillPublishJobBackupRow `json:"rows"`
}

// checksum 计算备份行的规范化 SHA-256（版本+行内容；不含可变的 RowCount/SHA256 字段本身）。
func (b *SkillPublishJobsBackup) checksum() (string, error) {
	canonical, err := json.Marshal(struct {
		Version int                        `json:"version"`
		Rows    []SkillPublishJobBackupRow `json:"rows"`
	}{Version: b.Version, Rows: b.Rows})
	if err != nil {
		return "", fmt.Errorf("marshal backup rows: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ExportSkillPublishJobs 从状态库导出全部发布任务行并计算校验和（BND-011：
// 迁移前备份；best-effort 读到表不存在视为空备份——目标库可能从未应用 000011）。
func (s *Storage) ExportSkillPublishJobs(ctx context.Context) (*SkillPublishJobsBackup, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	backup := &SkillPublishJobsBackup{
		Version: skillPublishJobsBackupVersion,
		Table:   skillPublishJobsTable,
		Rows:    make([]SkillPublishJobBackupRow, 0),
	}
	if !s.stateDB.Migrator().HasTable(skillPublishJobsTable) {
		backup.RowCount = 0
		sum, err := backup.checksum()
		if err != nil {
			return nil, err
		}
		backup.SHA256 = sum
		return backup, nil
	}
	rows, err := s.exportSkillPublishJobRows(ctx)
	if err != nil {
		return nil, err
	}
	backup.Rows = rows
	backup.RowCount = len(rows)
	sum, err := backup.checksum()
	if err != nil {
		return nil, err
	}
	backup.SHA256 = sum
	return backup, nil
}

// exportSkillPublishJobRows 用 raw SQL 读行（模型已随删除下线；列集与
// 000011/000017 结构锁定）。按 job_id 升序保证确定性备份。
func (s *Storage) exportSkillPublishJobRows(ctx context.Context) ([]SkillPublishJobBackupRow, error) {
	rows, err := s.stateDB.WithContext(ctx).Raw(
		`SELECT job_id, release_id, source_tenant, targets, idempotency_key, status, steps, created_at, updated_at
		 FROM skill_publish_jobs ORDER BY job_id ASC`).Rows()
	if err != nil {
		return nil, fmt.Errorf("export skill publish jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SkillPublishJobBackupRow, 0)
	for rows.Next() {
		var (
			entry     SkillPublishJobBackupRow
			targets   []byte
			steps     *[]byte
			createdAt backupTime
			updatedAt backupTime
		)
		if err := rows.Scan(&entry.JobID, &entry.ReleaseID, &entry.SourceTenant,
			&targets, &entry.IdempotencyKey, &entry.Status, &steps, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("export skill publish jobs scan: %w", err)
		}
		entry.Targets = json.RawMessage(targets)
		entry.CreatedAt = createdAt.Time
		entry.UpdatedAt = updatedAt.Time
		if steps != nil {
			entry.Steps = json.RawMessage(*steps)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export skill publish jobs rows: %w", err)
	}
	return out, nil
}

// backupTime 兼容多驱动的 timestamptz 扫描：postgres 返回 time.Time，
// sqlite（测试库）返回 string。
type backupTime struct{ time.Time }

func (b *backupTime) Scan(v interface{}) error {
	switch t := v.(type) {
	case time.Time:
		b.Time = t
		return nil
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
			parsed, err := time.Parse(layout, t)
			if err == nil {
				b.Time = parsed
				return nil
			}
		}
		return fmt.Errorf("unparseable timestamptz %q", t)
	case nil:
		return nil
	default:
		return fmt.Errorf("unsupported timestamptz type %T", v)
	}
}

// ImportSkillPublishJobs 从备份恢复存量行（BND-011：恢复须校验行数与 checksum；
// 表不存在视为前置未满足——必须先执行 000017 down 重建结构）。
func (s *Storage) ImportSkillPublishJobs(ctx context.Context, backup *SkillPublishJobsBackup) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if backup == nil || backup.Table != skillPublishJobsTable || backup.Version != skillPublishJobsBackupVersion {
		return fmt.Errorf("%w: unexpected backup shape", ErrSkillPublishJobsBackupInvalid)
	}
	if len(backup.Rows) != backup.RowCount {
		return fmt.Errorf("%w: row count mismatch (%d rows, header says %d)", ErrSkillPublishJobsBackupInvalid, len(backup.Rows), backup.RowCount)
	}
	sum, err := backup.checksum()
	if err != nil {
		return err
	}
	if sum != backup.SHA256 {
		return fmt.Errorf("%w: checksum mismatch (got %s, want %s)", ErrSkillPublishJobsBackupInvalid, sum, backup.SHA256)
	}
	if !s.stateDB.Migrator().HasTable(skillPublishJobsTable) {
		return fmt.Errorf("%w: table %s missing; run 000017 down to rebuild schema before import", ErrSkillPublishJobsBackupInvalid, skillPublishJobsTable)
	}
	return s.stateDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range backup.Rows {
			steps := interface{}(nil)
			if len(r.Steps) > 0 {
				steps = string(r.Steps)
			}
			result := tx.Exec(
				`INSERT INTO skill_publish_jobs (job_id, release_id, source_tenant, targets, idempotency_key, status, steps, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT (job_id) DO NOTHING`,
				r.JobID, r.ReleaseID, r.SourceTenant, string(r.Targets), r.IdempotencyKey, r.Status, steps, r.CreatedAt, r.UpdatedAt)
			if result.Error != nil {
				return fmt.Errorf("restore skill publish job %s: %w", r.JobID, result.Error)
			}
		}
		return nil
	})
}
