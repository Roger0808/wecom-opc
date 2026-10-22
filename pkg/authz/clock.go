// Package authz 承载 TASK-P06 的授权版本源读取、entitlement 校验辅助、
// 撤权失效通知通道与上游 token 撤销钩子（BND-110~112, REQ-003）。
//
// 版本语义（统一版本源）：
//   - 每个 subject 在状态库 authz_epochs 持有一行单调递增版本；
//   - 任何 entitlement 变更、identity 停用、revoke 都在同一事务里把版本 +1；
//   - runtime_grants 签发/续期把当前版本记入 authz_version，续期时版本不一致即拒绝；
//   - 读路径 CurrentVersion 支持「不缓存」或「≤5s 内存缓存」（Clock 可注入，便于测试）。
//
// 通知语义（BND-110）：撤权事务先提交，失效事件经进程内 Notifier 异步投递；
// 投递失败进 30s TTL 重试队列，失败绝不回滚已生效的拒绝。
//
// 上游 token 撤销（REQ-003）：TokenRevoker 接口化，P09 knowledge 桥就绪后可替换实现；
// 当前提供 no-op 与「删除 WeKnora 主库 auth_tokens 该行用户全部 token」的既有兼容实现。
package authz

import (
	"sync"
	"time"
)

// Clock 注入时钟：生产用 RealClock，测试用 FakeClock 断言 30s 租约 / 15s 宽容窗 / 5s 缓存预算。
type Clock interface {
	Now() time.Time
}

// RealClock 系统时钟。
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock 测试用手动时钟（非并发安全，测试单线程推进）。
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func (f *FakeClock) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
}
