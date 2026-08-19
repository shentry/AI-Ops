// Package memory 是 D13 的精确故障记忆：不依赖向量数据库，
// 键是 md5(group_key + 首条告警名)[:12] 的确定性指纹。
// 记忆命中不绕过 Guard、权限、审批和 Verify（GC-17）。
package memory

import (
	"crypto/md5"
	"encoding/hex"
)

// FaultFingerprint 计算故障记忆键。A11 的两个坑都在这里防住：
// 签名必须在诊断前可得（用 group_key + alertname，不是 RCA）；
// 它是确定性哈希，同一故障每次命中同一个键。
func FaultFingerprint(groupKey, alertName string) string {
	sum := md5.Sum([]byte(groupKey + alertName))
	return hex.EncodeToString(sum[:])[:12]
}
