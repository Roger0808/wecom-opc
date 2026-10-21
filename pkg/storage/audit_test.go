package storage

import (
	"context"
	"testing"

	"weknora-tokendance/pkg/model"
)

// 部署实测（PG jsonb 列）：空 before/after 写入会 22P02。归一为空对象后
// 必须能落库，保证审计永不因字段形态丢失。
func TestInsertAuditEventNormalizesEmptyJSON(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	migrateStateSchema(t, stateDB)
	err := s.InsertAuditEvent(context.Background(), &model.AuditEvent{
		Actor:  "tester",
		Action: "integration.test",
		Target: "audit_events",
	})
	if err != nil {
		t.Fatalf("insert with empty before/after: %v", err)
	}
	events, err := s.ListAuditEventsByAction(context.Background(), "integration.test")
	if err != nil || len(events) != 1 {
		t.Fatalf("read back: %v (%d)", err, len(events))
	}
	if events[0].Before != "{}" || events[0].After != "{}" {
		t.Fatalf("empty json not normalized: %+v", events[0])
	}
}
