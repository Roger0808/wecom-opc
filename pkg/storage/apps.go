package storage

import (
	"context"
	"errors"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CheckSlugAvailable 校验 slug 是否可用（未被占用）
func (s *Storage) CheckSlugAvailable(ctx context.Context, slug string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// CreateOrUpdateApp 创建或保存应用记录（支持同 slug 重新发布更新）
func (s *Storage) CreateOrUpdateApp(ctx context.Context, app *model.DeployedApp) error {
	var existing model.DeployedApp
	err := s.db.WithContext(ctx).Where("slug = ?", app.Slug).First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if app.ID == uuid.Nil {
				app.ID = uuid.New()
			}
			now := time.Now()
			app.CreatedAt = now
			app.UpdatedAt = now
			return s.db.WithContext(ctx).Create(app).Error
		}
		return err
	}

	// 存在时更新关键字段
	app.ID = existing.ID
	app.CreatedAt = existing.CreatedAt
	app.UpdatedAt = time.Now()
	return s.db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
		"name":               app.Name,
		"description":        app.Description,
		"app_url":            app.AppURL,
		"container_port":     app.ContainerPort,
		"status":             app.Status,
		"app_config_json":    app.AppConfigJSON,
		"resource_ids_json":  app.ResourceIDsJSON,
		"skill_name":         app.SkillName,
		"catalog_skill_id":   app.CatalogSkillID,
		"skill_synced":       app.SkillSynced,
		"build_log":          app.BuildLog,
		"error_message":      app.ErrorMessage,
		"created_by":         app.CreatedBy,
		"updated_at":         app.UpdatedAt,
	}).Error
}

// GetAppBySlug 根据唯一标识 slug 获取应用记录
func (s *Storage) GetAppBySlug(ctx context.Context, slug string) (*model.DeployedApp, error) {
	var app model.DeployedApp
	err := s.db.WithContext(ctx).Where("slug = ?", slug).First(&app).Error
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// ListApps 查询所有部署应用列表，按创建时间降序
func (s *Storage) ListApps(ctx context.Context) ([]model.DeployedApp, error) {
	var apps []model.DeployedApp
	err := s.db.WithContext(ctx).Order("created_at desc").Find(&apps).Error
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// UpdateAppStatus 更新应用的部署与技能同步状态
func (s *Storage) UpdateAppStatus(ctx context.Context, slug string, status string, buildLog string, errMsg string, catalogSkillID string, skillSynced bool) error {
	updates := map[string]interface{}{
		"status":        status,
		"build_log":     buildLog,
		"error_message": errMsg,
		"skill_synced":  skillSynced,
		"updated_at":    time.Now(),
	}
	if catalogSkillID != "" {
		updates["catalog_skill_id"] = catalogSkillID
	}
	return s.db.WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(updates).Error
}

// UpdateAppConfig 修改应用配置与绑定的线上资源，并更新时间戳
func (s *Storage) UpdateAppConfig(ctx context.Context, slug string, port int, appConfigJSON string, resourceIDsJSON string) error {
	return s.db.WithContext(ctx).Model(&model.DeployedApp{}).Where("slug = ?", slug).Updates(map[string]interface{}{
		"container_port":    port,
		"app_config_json":   appConfigJSON,
		"resource_ids_json": resourceIDsJSON,
		"updated_at":        time.Now(),
	}).Error
}
