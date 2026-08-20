package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeRetentionPolicyBoundsValues(t *testing.T) {
	got := NormalizeRetentionPolicy(RetentionPolicy{SnapshotDays: 0, CycleDays: 99999, AlertDays: -1, BackupDays: 7, BatchSize: 999999})
	if got.SnapshotDays != 90 || got.CycleDays != 3650 || got.AlertDays != 90 || got.BackupDays != 7 || got.BatchSize != 5000 {
		t.Fatalf("unexpected normalized policy: %+v", got)
	}
}

func TestRunRetentionDeletesExpiredParentsAndChildrenOnly(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339Nano)
	for _, captured := range []string{old, recent} {
		res, execErr := s.db.Exec(`INSERT INTO anthropic_snapshots(captured_at,raw_json,quota_count) VALUES(?,?,1)`, captured, "{}")
		if execErr != nil {
			t.Fatal(execErr)
		}
		id, _ := res.LastInsertId()
		if _, execErr = s.db.Exec(`INSERT INTO anthropic_quota_values(snapshot_id,quota_name,utilization) VALUES(?,?,?)`, id, "weekly", 10); execErr != nil {
			t.Fatal(execErr)
		}
	}
	for _, session := range []struct {
		id      string
		started string
	}{
		{"old-session", old},
		{"recent-session", recent},
	} {
		if _, err := s.db.Exec(`INSERT INTO sessions(id,provider,started_at,ended_at,poll_interval) VALUES(?,?,?,?,?)`, session.id, "synthetic", session.started, session.started, 120); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.RunRetention(RetentionPolicy{SnapshotDays: 30, CycleDays: 365, AlertDays: 30, BackupDays: 30, BatchSize: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedRows["anthropic_snapshots"] != 1 || report.DeletedRows["anthropic_quota_values"] != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.DeletedRows["sessions"] != 0 {
		t.Fatalf("365-day cycle retention must keep the 100-day-old session: %+v", report)
	}
	report, err = s.RunRetention(RetentionPolicy{SnapshotDays: 30, CycleDays: 30, AlertDays: 30, BackupDays: 30, BatchSize: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedRows["sessions"] != 1 {
		t.Fatalf("expired completed session was not pruned: %+v", report)
	}
	var snapshots, values int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM anthropic_snapshots`).Scan(&snapshots)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM anthropic_quota_values`).Scan(&values)
	if snapshots != 1 || values != 1 {
		t.Fatalf("remaining snapshots=%d values=%d", snapshots, values)
	}
	var sessions int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	if sessions != 1 {
		t.Fatalf("remaining sessions=%d, want 1", sessions)
	}
}

func TestBackupLifecycleAndPendingRestore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "onwatch.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("restore_probe", "before"); err != nil {
		t.Fatal(err)
	}
	info, err := s.CreateBackup(BackupReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes <= 0 || filepath.Dir(info.Path) != filepath.Join(dir, "backups") {
		t.Fatalf("unexpected backup: %+v", info)
	}
	if err := s.VerifyBackup(info.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("restore_probe", "after"); err != nil {
		t.Fatal(err)
	}
	if err := s.StageRestore(info.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBackup(info.Name); err == nil {
		t.Fatal("staged restore backup must not be deletable before restart")
	}
	if _, err := os.Stat(dbPath + pendingRestoreSuffix); err != nil {
		t.Fatalf("restore marker missing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyPendingRestore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.RollbackPath == "" {
		t.Fatalf("unexpected restore result: %+v", result)
	}
	rollbackDB, err := sql.Open("sqlite", result.RollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	var rollbackValue string
	if err := rollbackDB.QueryRow(`SELECT value FROM settings WHERE key='restore_probe'`).Scan(&rollbackValue); err != nil {
		_ = rollbackDB.Close()
		t.Fatal(err)
	}
	if err := rollbackDB.Close(); err != nil {
		t.Fatal(err)
	}
	if rollbackValue != "after" {
		t.Fatalf("rollback value=%q, want pre-restore live value", rollbackValue)
	}
	restored, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.GetSetting("restore_probe")
	if err != nil || got != "before" {
		t.Fatalf("restored value=%q err=%v", got, err)
	}
}

func TestBackupNamesCannotEscapeBackupDirectory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "onwatch.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.VerifyBackup(`..\secret.db`); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if err := s.StageRestore("../secret.db"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestDatabaseMaintenanceStatusIsBoundedAndUseful(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "onwatch.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status, err := s.MaintenanceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.DatabaseBytes <= 0 || status.PageSize <= 0 || status.PageCount <= 0 || status.BackupDirectory == "" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if s.SchemaVersion() <= 0 {
		t.Fatalf("schema version=%d, want a positive managed version", s.SchemaVersion())
	}
	if err := s.CheckHealth(); err != nil {
		t.Fatalf("database health check failed: %v", err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceDuePersistsAcrossRestarts(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	if !s.MaintenanceDue(now, 24*time.Hour) {
		t.Fatal("maintenance with no prior completion must be due")
	}
	if err := s.MarkMaintenanceCompleted(now); err != nil {
		t.Fatal(err)
	}
	if s.MaintenanceDue(now.Add(23*time.Hour), 24*time.Hour) {
		t.Fatal("maintenance must not repeat within 24 hours")
	}
	if !s.MaintenanceDue(now.Add(25*time.Hour), 24*time.Hour) {
		t.Fatal("maintenance must become due after 24 hours")
	}
}

func TestAlertLifecycleAndRecoveryConfirmation(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.CreateSystemAlert("opencode", "auth_error", "认证失效", "需要更新 Cookie", "error", `{"account_id":"7"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeSystemAlert(id); err != nil {
		t.Fatal(err)
	}
	alerts, err := s.GetActiveSystemAlerts()
	if err != nil || len(alerts) != 1 || alerts[0].Status != "acknowledged" {
		t.Fatalf("alerts=%+v err=%v", alerts, err)
	}
	if err = s.SilenceSystemAlert(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	alerts, err = s.GetActiveSystemAlerts()
	if err != nil || len(alerts) != 0 {
		t.Fatalf("silenced alert should be hidden: %+v err=%v", alerts, err)
	}
	confirmed, err := s.RecordAuthRecoverySuccess("opencode", "7", 2)
	if err != nil || confirmed {
		t.Fatalf("first recovery confirmation=%v err=%v", confirmed, err)
	}
	confirmed, err = s.RecordAuthRecoverySuccess("opencode", "7", 2)
	if err != nil || !confirmed {
		t.Fatalf("second recovery confirmation=%v err=%v", confirmed, err)
	}
	resolved, err := s.ResolveSystemAlertsForAccount("opencode", "7")
	if err != nil || resolved != 1 {
		t.Fatalf("resolved=%d err=%v", resolved, err)
	}
}

func TestAuthRecoveryDoesNotPersistWithoutActiveIncident(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	confirmed, err := s.RecordAuthRecoverySuccess("opencode", "7", 2)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Fatal("success without an active auth incident should not confirm recovery")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_recovery_state WHERE provider=? AND account_id=?`, "opencode", "7").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recovery state rows=%d, want 0", count)
	}
}

func TestCollectionHealthKeepsOpenCodeAccountsIsolated(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.CreateOpenCodeAccount("first", "workspace-first", "cookie-first", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateOpenCodeAccount("second", "workspace-second", "cookie-second", true)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.UpdateOpenCodeAccountPollState(first.AccountID, first.CredentialVersion, OpenCodeAuthValid, "", true, time.Now().Add(time.Minute))
	_, _ = s.UpdateOpenCodeAccountPollState(second.AccountID, second.CredentialVersion, OpenCodeAuthError, "network", false, time.Now().Add(time.Minute))
	items, err := s.QueryCollectionHealth(2*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]CollectionHealth{}
	for _, item := range items {
		if item.Provider == "opencode" {
			seen[item.AccountID] = item
		}
	}
	if seen[first.AccountID].Status != "healthy" || seen[second.AccountID].Status != "error" || seen[second.AccountID].ErrorCode != "network" {
		t.Fatalf("isolated health rows=%+v", seen)
	}
}
