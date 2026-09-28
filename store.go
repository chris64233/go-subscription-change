package gosubscriptionchange

import (
	"sort"
	"sync"
)

// storeData 是仓储在某一时刻的完整快照。事务在快照的深拷贝上执行，
// 提交时整包替换，读事务拿到的永远是一致快照；
// 回调返回错误则整笔回滚，不会留下半成品状态。
type storeData struct {
	plans   map[string]*Plan
	subs    map[string]*Subscription
	changes map[string]*ChangeApplication
	bills   map[string]*Bill
	events  []*Event
	// 唯一索引
	billByChange  map[string]string // changeID -> billID
	notifByChange map[string]*Notification
	changeByToken map[string]string // 外部申请号 -> changeID
	seq           int64
}

func newStoreData() *storeData {
	return &storeData{
		plans:         map[string]*Plan{},
		subs:          map[string]*Subscription{},
		changes:       map[string]*ChangeApplication{},
		bills:         map[string]*Bill{},
		billByChange:  map[string]string{},
		notifByChange: map[string]*Notification{},
		changeByToken: map[string]string{},
	}
}

// clone 深拷贝整份数据。
func (d *storeData) clone() *storeData {
	c := newStoreData()
	c.seq = d.seq
	for id, p := range d.plans {
		pc := *p
		pc.Prices = append([]PriceVersion(nil), p.Prices...)
		c.plans[id] = &pc
	}
	for id, s := range d.subs {
		sc := *s
		c.subs[id] = &sc
	}
	for id, ch := range d.changes {
		chc := *ch
		c.changes[id] = &chc
	}
	for id, b := range d.bills {
		bc := *b
		c.bills[id] = &bc
	}
	for k, v := range d.billByChange {
		c.billByChange[k] = v
	}
	for k, v := range d.notifByChange {
		n := *v
		c.notifByChange[k] = &n
	}
	for k, v := range d.changeByToken {
		c.changeByToken[k] = v
	}
	c.events = make([]*Event, len(d.events))
	for i, e := range d.events {
		ec := *e
		c.events[i] = &ec
	}
	return c
}

// Tx 是一次事务的句柄，仅在回调执行期间有效。
type Tx struct {
	d *storeData
}

// nextSeq 生成仓储内单调自增序号，可同时用作 ID 与事件序号。
func (tx *Tx) nextSeq() int64 {
	tx.d.seq++
	return tx.d.seq
}

// newID 生成形如 prefix-000001 的确定性 ID（同实例内唯一）。
func (tx *Tx) newID(prefix string) string {
	return idString(prefix, tx.nextSeq())
}

// ---- 计划 ----

func (tx *Tx) getPlan(id string) (*Plan, bool) {
	p, ok := tx.d.plans[id]
	if !ok {
		return nil, false
	}
	pc := *p
	pc.Prices = append([]PriceVersion(nil), p.Prices...)
	return &pc, true
}

func (tx *Tx) putPlan(p *Plan) {
	pc := *p
	pc.Prices = append([]PriceVersion(nil), p.Prices...)
	tx.d.plans[p.ID] = &pc
}

func (tx *Tx) listPlans() []*Plan {
	out := make([]*Plan, 0, len(tx.d.plans))
	for _, p := range tx.d.plans {
		pc := *p
		pc.Prices = append([]PriceVersion(nil), p.Prices...)
		out = append(out, &pc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- 订阅 ----

func (tx *Tx) getSubscription(id string) (*Subscription, bool) {
	s, ok := tx.d.subs[id]
	if !ok {
		return nil, false
	}
	sc := *s
	return &sc, true
}

// saveSubscription 以乐观锁写入订阅：expectedVersion 必须与当前版本一致。
// 新对象 expectedVersion 传 0。
func (tx *Tx) saveSubscription(s *Subscription, expectedVersion int64) error {
	cur, ok := tx.d.subs[s.ID]
	if ok {
		if cur.Version != expectedVersion {
			return ErrConcurrentUpdate
		}
		s.Version = cur.Version + 1
	} else {
		if expectedVersion != 0 {
			return ErrConcurrentUpdate
		}
		s.Version = 1
	}
	sc := *s
	tx.d.subs[s.ID] = &sc
	return nil
}

func (tx *Tx) listSubscriptions() []*Subscription {
	out := make([]*Subscription, 0, len(tx.d.subs))
	for _, s := range tx.d.subs {
		sc := *s
		out = append(out, &sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- 变更申请 ----

func (tx *Tx) getChange(id string) (*ChangeApplication, bool) {
	ch, ok := tx.d.changes[id]
	if !ok {
		return nil, false
	}
	chc := *ch
	return &chc, true
}

// changeByClientToken 按外部申请号查找申请。
func (tx *Tx) changeByClientToken(token string) (*ChangeApplication, bool) {
	id, ok := tx.d.changeByToken[token]
	if !ok {
		return nil, false
	}
	return tx.getChange(id)
}

// saveChange 以乐观锁写入变更申请。
func (tx *Tx) saveChange(ch *ChangeApplication, expectedVersion int64) error {
	cur, ok := tx.d.changes[ch.ID]
	if ok {
		if cur.Version != expectedVersion {
			return ErrConcurrentUpdate
		}
		ch.Version = cur.Version + 1
	} else {
		if expectedVersion != 0 {
			return ErrConcurrentUpdate
		}
		ch.Version = 1
		if ch.ClientToken != "" {
			tx.d.changeByToken[ch.ClientToken] = ch.ID
		}
	}
	chc := *ch
	tx.d.changes[ch.ID] = &chc
	return nil
}

// ---- 账单（changeID 唯一）----

// insertBill 写入账单；同一 changeID 只允许一张账单，重复插入报冲突，
// 保证同一笔变更无论被推进多少次都只收一次费。
func (tx *Tx) insertBill(b *Bill) error {
	if _, ok := tx.d.bills[b.ID]; ok {
		return conflictf("bill %s already exists", b.ID)
	}
	if b.ChangeID != "" {
		if _, exists := tx.d.billByChange[b.ChangeID]; exists {
			return conflictf("settlement bill already exists for change %s", b.ChangeID)
		}
	}
	bc := *b
	tx.d.bills[b.ID] = &bc
	if b.ChangeID != "" {
		tx.d.billByChange[b.ChangeID] = b.ID
	}
	return nil
}

func (tx *Tx) listBills(subscriptionID string) []*Bill {
	out := make([]*Bill, 0)
	for _, b := range tx.d.bills {
		if b.SubscriptionID == subscriptionID {
			bc := *b
			out = append(out, &bc)
		}
	}
	// map 遍历无序，按创建时间、再按 ID 输出稳定顺序（即创建顺序）。
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---- 通知（changeID 唯一）----

// insertNotification 写入生效通知；同一 changeID 只允许一条。
func (tx *Tx) insertNotification(n *Notification) error {
	if _, exists := tx.d.notifByChange[n.ChangeID]; exists {
		return conflictf("notification already exists for change %s", n.ChangeID)
	}
	nc := *n
	tx.d.notifByChange[n.ChangeID] = &nc
	return nil
}

// ---- 历史事件 ----

func (tx *Tx) appendEvent(e *Event) {
	e.Seq = tx.nextSeq()
	ec := *e
	tx.d.events = append(tx.d.events, &ec)
}

func (tx *Tx) listEvents(subscriptionID string) []*Event {
	out := make([]*Event, 0, len(tx.d.events))
	for _, e := range tx.d.events {
		if e.SubscriptionID == subscriptionID {
			ec := *e
			out = append(out, &ec)
		}
	}
	return out
}

// Store 是线程安全的内存仓储。所有业务操作都必须通过 Update/View，
// 互斥锁 + 快照拷贝保证“读一致、写原子”。
type Store struct {
	mu sync.Mutex
	d  *storeData
}

// NewStore 创建空仓储。
func NewStore() *Store {
	return &Store{d: newStoreData()}
}

// Update 在一个原子事务中执行 fn：fn 返回 nil 才提交，否则整体回滚。
func (s *Store) Update(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.d.clone()
	tx := &Tx{d: work}
	if err := fn(tx); err != nil {
		return err // 丢弃 work，即回滚
	}
	s.d = work
	return nil
}

// View 在只读事务中执行 fn，拿到的是当前一致快照。
func (s *Store) View(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Tx{d: s.d} // 所有 getter 均返回拷贝，调用方无法改到内部状态
	return fn(tx)
}
