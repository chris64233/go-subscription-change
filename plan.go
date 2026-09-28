package gosubscriptionchange

import "time"

// PriceVersion 是套餐价格的一个不可变版本。
// 新版本价格只对“新版本生效之后受理”的申请可见；
// 已经受理的变更申请冻结了当时的价格快照，历史价格变化无法改写它。
type PriceVersion struct {
	Version     int       `json:"version"`
	Amount      int64     `json:"amount"` // 每计费周期价格（最小货币单位，非负）
	EffectiveAt time.Time `json:"effectiveAt"`
}

// Plan 是套餐配置。已发布的价格版本永不删除、不可修改。
type Plan struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Currency  string         `json:"currency"` // ISO-4217，如 CNY
	Prices    []PriceVersion `json:"prices"`
	CreatedAt time.Time      `json:"createdAt"`
}

// priceAt 返回 at 时刻适用的价格版本。
// 取 EffectiveAt <= at 中版本号最大（即最新）的一个。
func (p *Plan) priceAt(at time.Time) (PriceVersion, bool) {
	var best *PriceVersion
	for i := range p.Prices {
		v := &p.Prices[i]
		if !v.EffectiveAt.After(at) && (best == nil || v.Version > best.Version) {
			best = v
		}
	}
	if best == nil {
		return PriceVersion{}, false
	}
	return *best, true
}

// CurrentPrice 返回最新价格版本（无论生效时间）。
func (p *Plan) CurrentPrice() (PriceVersion, bool) {
	var best *PriceVersion
	for i := range p.Prices {
		if best == nil || p.Prices[i].Version > best.Version {
			best = &p.Prices[i]
		}
	}
	if best == nil {
		return PriceVersion{}, false
	}
	return *best, true
}
