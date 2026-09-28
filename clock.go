package gosubscriptionchange

import (
	"context"
	"time"
)

// Clock 抽象时间来源，生产环境使用 SystemClock，测试可注入固定时钟。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用本机 UTC 时间。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FakeClock 测试用固定/可推进时钟。
type FakeClock struct {
	t time.Time
}

// NewFakeClock 基于给定时间创建测试时钟（归一化为 UTC）。
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{t: t.UTC()}
}

// Now 返回当前冻结时间。
func (c *FakeClock) Now() time.Time { return c.t }

// Advance 将时钟向前推进 d。
func (c *FakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// Set 直接设置时钟时间（归一化为 UTC）。
func (c *FakeClock) Set(t time.Time) { c.t = t.UTC() }

type ctxKey int

const clockKey ctxKey = iota

// withClock 把时钟放入 context（测试用）。
func withClock(ctx context.Context, c Clock) context.Context {
	return context.WithValue(ctx, clockKey, c)
}

// clockFrom 从 context 取时钟，缺省使用系统时钟。
func clockFrom(ctx context.Context) Clock {
	if c, ok := ctx.Value(clockKey).(Clock); ok && c != nil {
		return c
	}
	return SystemClock{}
}
