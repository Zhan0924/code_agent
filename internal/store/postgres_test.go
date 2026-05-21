package store

import (
	"testing"
)

func TestPostgresConfig_DSN(t *testing.T) {
	cfg := &PostgresConfig{
		Host:     "localhost",
		Port:     5432,
		User:     "agent",
		Password: "secret",
		Database: "code_agent",
		SSLMode:  "disable",
	}

	dsn := cfg.DSN()
	expected := "host=localhost port=5432 user=agent password=secret dbname=code_agent sslmode=disable"
	if dsn != expected {
		t.Errorf("DSN mismatch:\ngot:  %s\nwant: %s", dsn, expected)
	}
}

func TestTaskRecord_Fields(t *testing.T) {
	r := &TaskRecord{
		ID:        "task-1",
		SessionID: "sess-1",
		UserID:    "user-1",
		Intent:    "code_query",
		State:     "pending",
		UserInput: "explain this function",
	}

	if r.ID != "task-1" {
		t.Errorf("expected task-1, got %s", r.ID)
	}
	if r.State != "pending" {
		t.Errorf("expected pending, got %s", r.State)
	}
}

func TestAuditRecord_Fields(t *testing.T) {
	r := &AuditRecord{
		TaskID:    "task-1",
		UserID:    "user-1",
		Action:    "execute_code",
		RiskLevel: "high",
	}
	if r.RiskLevel != "high" {
		t.Errorf("expected high, got %s", r.RiskLevel)
	}
}

func TestApprovalRecord_Fields(t *testing.T) {
	r := &ApprovalRecord{
		TaskID:    "task-1",
		SessionID: "sess-1",
		Action:    "kubectl apply",
		RiskLevel: "high",
		Status:    "pending",
	}
	if r.Status != "pending" {
		t.Errorf("expected pending, got %s", r.Status)
	}
}
