package subscription

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store 是事件溯源存储：所有状态变更先原子追加到 WAL（JSON Lines），
// 再投影到内存状态；空路径表示纯内存模式。一次 Mutate 提交的事件组
// 要么全部落盘并生效，要么全部不发生——到期推进（切换套餐 + 账单 +
// 通知）依赖这一原子边界。
type Store struct {
	mu   sync.Mutex
	st   *state
	f    *os.File
	path string
	seq  int64
}

// Open 打开（必要时创建）WAL 文件并重放历史。path 为空时为纯内存存储。
func Open(path string) (*Store, error) {
	s := &Store{st: newState(), path: path}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("subscription: create wal dir: %w", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := s.replay(data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("subscription: read wal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("subscription: open wal: %w", err)
	}
	s.f = f
	return s, nil
}

func (s *Store) replay(data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("subscription: replay wal: %w", err)
		}
		s.st.apply(e)
		if e.Seq > s.seq {
			s.seq = e.Seq
		}
	}
	return sc.Err()
}

// Close 刷盘并关闭 WAL。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		if err := s.f.Sync(); err != nil {
			return err
		}
		return s.f.Close()
	}
	return nil
}

// View 在一致快照上执行只读操作。
func (s *Store) View(fn func(View) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(viewOf(s.st))
}

// Mutate 在一次原子事务中执行读-判定-事件提交：fn 基于当前视图校验并
// 返回事件组；返回错误则整个事务中止、无任何副作用。事件先全部追加
// WAL 并 fsync，再顺序投影到内存。
func (s *Store) Mutate(fn func(View) ([]Event, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	events, err := fn(viewOf(s.st))
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	if s.f != nil {
		buf := make([]byte, 0, 4096)
		for i := range events {
			events[i].Seq = s.seq + int64(i) + 1
			line, err := json.Marshal(events[i])
			if err != nil {
				return fmt.Errorf("subscription: encode event: %w", err)
			}
			buf = append(buf, line...)
			buf = append(buf, '\n')
		}
		if _, err := s.f.Write(buf); err != nil {
			return fmt.Errorf("subscription: write wal: %w", err)
		}
		if err := s.f.Sync(); err != nil {
			return fmt.Errorf("subscription: sync wal: %w", err)
		}
	}
	if s.f == nil {
		// 内存模式：序号基于组前 base 一次性计算，避免循环内读到已更新的 s.seq。
		base := s.seq
		for i := range events {
			events[i].Seq = base + int64(i) + 1
		}
	}
	for i := range events {
		s.seq = events[i].Seq
		s.st.apply(events[i])
	}
	return nil
}

// View 是事务内的只读状态访问接口，所有返回值均为防御性拷贝。
type View struct {
	st *state
}

func viewOf(st *state) View { return View{st: st} }

// Plan 返回套餐配置拷贝。
func (v View) Plan(id string) (*Plan, bool) {
	p, ok := v.st.plans[id]
	if !ok {
		return nil, false
	}
	return clonePlan(p), true
}

// Plans 返回全部套餐（按 ID 排序）。
func (v View) Plans() []*Plan {
	out := make([]*Plan, 0, len(v.st.plans))
	for _, p := range v.st.plans {
		out = append(out, clonePlan(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Subscription 返回订阅拷贝。
func (v View) Subscription(id string) (*Subscription, bool) {
	sub, ok := v.st.subs[id]
	if !ok {
		return nil, false
	}
	return cloneSub(sub), true
}

// Change 返回变更申请拷贝。
func (v View) Change(id string) (*ChangeRequest, bool) {
	c, ok := v.st.changes[id]
	if !ok {
		return nil, false
	}
	return cloneChange(c), true
}

// PendingChange 返回订阅当前待生效变更（含状态校验）。
func (v View) PendingChange(subID string) (*ChangeRequest, bool) {
	sub, ok := v.st.subs[subID]
	if !ok || sub.PendingChangeID == "" {
		return nil, false
	}
	c, ok := v.st.changes[sub.PendingChangeID]
	if !ok {
		return nil, false
	}
	return cloneChange(c), true
}

// ChangesBySubscription 返回订阅的全部变更历史（按版本顺序）。
func (v View) ChangesBySubscription(subID string) []*ChangeRequest {
	ids := v.st.subChanges[subID]
	out := make([]*ChangeRequest, 0, len(ids))
	for _, id := range ids {
		if c, ok := v.st.changes[id]; ok {
			out = append(out, cloneChange(c))
		}
	}
	return out
}

// ChangeByExternal 按外部申请号查询（仅索引未取消/有效的申请）。
func (v View) ChangeByExternal(externalID string) (*ChangeRequest, bool) {
	id, ok := v.st.externalIdx[externalID]
	if !ok {
		return nil, false
	}
	c, ok := v.st.changes[id]
	if !ok {
		return nil, false
	}
	return cloneChange(c), true
}

// Bill 返回账单拷贝。
func (v View) Bill(id string) (*Bill, bool) {
	b, ok := v.st.bills[id]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

// BillsBySubscription 返回订阅账单（按时间顺序）。
func (v View) BillsBySubscription(subID string) []*Bill {
	out := make([]*Bill, 0)
	for _, b := range v.st.bills {
		if b.SubscriptionID == subID {
			cp := *b
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// BillByChange 返回变更申请已出的账单（advance 幂等判定）。
func (v View) BillByChange(changeID string) (*Bill, bool) {
	id, ok := v.st.changeBills[changeID]
	if !ok {
		return nil, false
	}
	b, ok := v.st.bills[id]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

// NotificationByDedup 查询去重键对应的通知。
func (v View) NotificationByDedup(key string) (*Notification, bool) {
	id, ok := v.st.notifDedup[key]
	if !ok {
		return nil, false
	}
	n, ok := v.st.notifs[id]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

func clonePlan(p *Plan) *Plan {
	cp := *p
	cp.Versions = append([]PlanVersion(nil), p.Versions...)
	return &cp
}

func cloneSub(s *Subscription) *Subscription {
	cp := *s
	return &cp
}

func cloneChange(c *ChangeRequest) *ChangeRequest {
	cp := *c
	return &cp
}
