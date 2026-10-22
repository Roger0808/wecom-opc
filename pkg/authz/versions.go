package authz

import (
	"context"
	"sync"
	"time"

	"weknora-tokendance/pkg/storage"
)

// VersionSource 授权版本读路径（P06.1）：从状态库 authz_epochs 读取 CurrentVersion(subject)。
// cacheTTL == 0 表示不缓存（网关拒绝路径的默认行为：无缓存 + 事务先提交，BND-110 预算天然满足）；
// cacheTTL > 0 启用内存缓存（上限 5s），缓存新鲜度以注入 Clock 判定，供高频读路径选择。
type VersionSource struct {
	storage  *storage.Storage
	clock    Clock
	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cachedVersion
}

type cachedVersion struct {
	version int64
	at      time.Time
}

// NewVersionSource 构造版本读路径；ttl 传 0 关闭缓存。
func NewVersionSource(s *storage.Storage, clock Clock, ttl time.Duration) *VersionSource {
	if clock == nil {
		clock = RealClock{}
	}
	return &VersionSource{
		storage:  s,
		clock:    clock,
		cacheTTL: ttl,
		cache:    make(map[string]cachedVersion),
	}
}

// Current 读取当前版本（带可选缓存）。
func (v *VersionSource) Current(ctx context.Context, subject string) (int64, error) {
	if v.cacheTTL > 0 {
		now := v.clock.Now()
		v.mu.Lock()
		if cv, ok := v.cache[subject]; ok && now.Sub(cv.at) < v.cacheTTL {
			v.mu.Unlock()
			return cv.version, nil
		}
		v.mu.Unlock()
	}
	version, err := v.storage.CurrentAuthzVersion(ctx, subject)
	if err != nil {
		return 0, err
	}
	if v.cacheTTL > 0 {
		v.mu.Lock()
		v.cache[subject] = cachedVersion{version: version, at: v.clock.Now()}
		v.mu.Unlock()
	}
	return version, nil
}

// CurrentFresh 绕过缓存直读数据库：签发/续期/撤权后的判定路径必须用它，
// 保证「revoke 事务提交后立即拒绝」不被 5s 缓存窗口吞掉。
func (v *VersionSource) CurrentFresh(ctx context.Context, subject string) (int64, error) {
	return v.storage.CurrentAuthzVersion(ctx, subject)
}
