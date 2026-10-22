package authz

import (
	"context"
	"log"
	"sync"
	"time"
)

// InvalidateEvent 撤权失效事件（revoke 事务提交后投递，BND-110）。
type InvalidateEvent struct {
	Subject  string    `json:"subject"`
	TenantID string    `json:"tenant_id,omitempty"`
	Reason   string    `json:"reason"`
	Version  int64     `json:"version"`
	At       time.Time `json:"at"`
}

// Channel 失效通知通道接口（进程内实现；P09 起可换 Redis/MQ 实现，接口保持不变）。
type Channel interface {
	Name() string
	Notify(ctx context.Context, ev InvalidateEvent) error
}

const (
	defaultRetryInterval = time.Second
	defaultRetryTTL      = 30 * time.Second // BND-110：重试队列 TTL
)

type retryEntry struct {
	ev          InvalidateEvent
	channel     string
	nextAttempt time.Time
	expiresAt   time.Time
}

// Notifier 失效通知中心：维护订阅者 map + 30s TTL 重试队列（P06.3，进程内实现）。
// 投递失败只入重试队列，绝不回滚已提交的撤权（通知失败不影响拒绝生效）。
type Notifier struct {
	mu            sync.Mutex
	clock         Clock
	channels      map[string]Channel
	retryInterval time.Duration
	retryTTL      time.Duration
	retries       []retryEntry
}

// NewNotifier 构造通知中心。
func NewNotifier(clock Clock) *Notifier {
	if clock == nil {
		clock = RealClock{}
	}
	return &Notifier{
		clock:         clock,
		channels:      make(map[string]Channel),
		retryInterval: defaultRetryInterval,
		retryTTL:      defaultRetryTTL,
	}
}

// Register 登记通知通道（同名校验重复，后注册者覆盖前者）。
func (n *Notifier) Register(ch Channel) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.channels[ch.Name()] = ch
}

// Invalidate 同步向全部已登记通道投递事件；失败事件进入 30s TTL 重试队列。
// 本方法不返回错误：撤权已提交，通知尽力送达。
func (n *Notifier) Invalidate(ctx context.Context, ev InvalidateEvent) {
	n.mu.Lock()
	channels := make([]Channel, 0, len(n.channels))
	for _, ch := range n.channels {
		channels = append(channels, ch)
	}
	n.mu.Unlock()

	now := n.clock.Now()
	for _, ch := range channels {
		if err := ch.Notify(ctx, ev); err != nil {
			log.Printf("[Authz] invalidate notify failed (channel=%s, subject=%s): %v", ch.Name(), ev.Subject, err)
			n.enqueueRetry(ev, ch.Name(), now)
		}
	}
}

func (n *Notifier) enqueueRetry(ev InvalidateEvent, channel string, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.retries = append(n.retries, retryEntry{
		ev:          ev,
		channel:     channel,
		nextAttempt: now.Add(n.retryInterval),
		expiresAt:   now.Add(n.retryTTL),
	})
}

// DispatchDue 投递所有到期且未过期的重试事件（Start 循环与测试断言共用同一入口）。
func (n *Notifier) DispatchDue(now time.Time) {
	n.mu.Lock()
	due := make([]retryEntry, 0, len(n.retries))
	keep := n.retries[:0]
	for _, r := range n.retries {
		switch {
		case now.Before(r.nextAttempt):
			keep = append(keep, r)
		case !now.Before(r.expiresAt):
			// 超过 TTL：丢弃（BND-110 允许通知丢失，拒绝已生效）。
		default:
			due = append(due, retryEntry{
				ev:          r.ev,
				channel:     r.channel,
				nextAttempt: now.Add(n.retryInterval),
				expiresAt:   r.expiresAt,
			})
		}
	}
	n.retries = keep
	channels := make(map[string]Channel, len(n.channels))
	for name, ch := range n.channels {
		channels[name] = ch
	}
	n.mu.Unlock()

	for _, r := range due {
		ch, ok := channels[r.channel]
		if !ok {
			continue
		}
		if err := ch.Notify(context.Background(), r.ev); err != nil {
			log.Printf("[Authz] invalidate retry failed (channel=%s, subject=%s): %v", r.channel, r.ev.Subject, err)
			n.mu.Lock()
			n.retries = append(n.retries, r)
			n.mu.Unlock()
		}
	}
}

// Start 启动后台重试循环（生产用）；ctx 取消即退出。
func (n *Notifier) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(n.retryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n.DispatchDue(n.clock.Now())
			}
		}
	}()
}

// PendingRetries 返回重试队列长度（测试可观测性）。
func (n *Notifier) PendingRetries() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.retries)
}
