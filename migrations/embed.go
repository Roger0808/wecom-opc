// Package migrations 承载平台状态库的版本化迁移 SQL（启动时由 storage.RunMigrations 按序执行）。
// 命名规范：<version>_<name>.up.sql / <version>_<name>.down.sql。
// 迁移只追加语义：已应用版本永不修改，回滚脚本仅供全新环境重建。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
