package gosubscriptionchange

import "fmt"

// idString 由前缀与仓储自增序号拼出确定性 ID，例如 change-000042。
func idString(prefix string, seq int64) string {
	return fmt.Sprintf("%s-%06d", prefix, seq)
}
