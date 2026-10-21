package kbridge

import "encoding/json"

// auditJSON 序列化审计 detail（best-effort，失败返回空对象字符串）。
func auditJSON(detail map[string]interface{}) string {
	raw, err := json.Marshal(detail)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
