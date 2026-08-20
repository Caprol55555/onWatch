package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	SettingRetentionPolicy = "retention_policy"
	SettingAlertLifecycle  = "alert_lifecycle"
	SettingLastMaintenance = "last_maintenance_at"
	pendingRestoreSuffix   = ".restore-request.json"
	BackupReasonManual     = "manual"
	BackupReasonRetention  = "retention"
	BackupReasonRestore    = "pre-restore"
)

var ErrBackupStagedForRestore = errors.New("backup is staged for restore")

func (s *Store) MaintenanceDue(now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	raw, err := s.GetSetting(SettingLastMaintenance)
	if err != nil || strings.TrimSpace(raw) == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return true
	}
	return !now.UTC().Before(last.UTC().Add(interval))
}

func (s *Store) MarkMaintenanceCompleted(at time.Time) error {
	return s.SetSetting(SettingLastMaintenance, at.UTC().Format(time.RFC3339Nano))
}

// RetentionPolicy bounds long-lived telemetry without introducing a resident
// maintenance service. RunRetention performs one small transaction per call.
type RetentionPolicy struct {
	SnapshotDays int `json:"snapshot_days"`
	CycleDays    int `json:"cycle_days"`
	AlertDays    int `json:"alert_days"`
	BackupDays   int `json:"backup_days"`
	BatchSize    int `json:"batch_size"`
}

// AlertLifecyclePolicy controls confirmation and temporary suppression while
// preserving the existing notification channel and threshold settings.
type AlertLifecyclePolicy struct {
	FailureConfirmations  int `json:"failure_confirmations"`
	RecoveryConfirmations int `json:"recovery_confirmations"`
	SilenceMinutes        int `json:"silence_minutes"`
}

func DefaultAlertLifecyclePolicy() AlertLifecyclePolicy {
	return AlertLifecyclePolicy{FailureConfirmations: 2, RecoveryConfirmations: 2, SilenceMinutes: 60}
}

func NormalizeAlertLifecyclePolicy(p AlertLifecyclePolicy) AlertLifecyclePolicy {
	d := DefaultAlertLifecyclePolicy()
	if p.FailureConfirmations <= 0 {
		p.FailureConfirmations = d.FailureConfirmations
	}
	if p.RecoveryConfirmations <= 0 {
		p.RecoveryConfirmations = d.RecoveryConfirmations
	}
	if p.SilenceMinutes <= 0 {
		p.SilenceMinutes = d.SilenceMinutes
	}
	if p.FailureConfirmations > 10 {
		p.FailureConfirmations = 10
	}
	if p.RecoveryConfirmations > 10 {
		p.RecoveryConfirmations = 10
	}
	if p.SilenceMinutes > 10080 {
		p.SilenceMinutes = 10080
	}
	return p
}

func (s *Store) AlertLifecyclePolicy() AlertLifecyclePolicy {
	p := DefaultAlertLifecyclePolicy()
	if raw, err := s.GetSetting(SettingAlertLifecycle); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return NormalizeAlertLifecyclePolicy(p)
}

func (s *Store) ResetAuthRecovery(provider, accountID string) error {
	_, err := s.db.Exec(`DELETE FROM auth_recovery_state WHERE provider=? AND account_id=?`, provider, accountID)
	return err
}

func (s *Store) ResetAuthFailure(provider, accountID string) error {
	_, err := s.db.Exec(`DELETE FROM auth_failure_state WHERE provider=? AND account_id=?`, provider, accountID)
	return err
}

func (s *Store) RecordAuthFailure(provider, accountID, errorCode string, required int) (bool, error) {
	if required < 1 {
		required = 1
	}
	if required > 10 {
		required = 10
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO auth_failure_state(provider,account_id,error_code,failure_count,updated_at)
		VALUES(?,?,?,1,?)
		ON CONFLICT(provider,account_id) DO UPDATE SET
			error_code=excluded.error_code,
			failure_count=CASE WHEN auth_failure_state.error_code=excluded.error_code THEN auth_failure_state.failure_count+1 ELSE 1 END,
			updated_at=excluded.updated_at
	`, provider, accountID, errorCode, now)
	if err != nil {
		return false, err
	}
	var count int
	if err := s.db.QueryRow(`SELECT failure_count FROM auth_failure_state WHERE provider=? AND account_id=?`, provider, accountID).Scan(&count); err != nil {
		return false, err
	}
	return count >= required, nil
}

func (s *Store) RecordAuthRecoverySuccess(provider, accountID string, required int) (bool, error) {
	hasIncident, err := s.HasActiveAuthAlertForAccount(provider, accountID)
	if err != nil {
		return false, err
	}
	if !hasIncident {
		// Normal successful polls must not turn into a permanent write stream.
		// Clear any orphaned counter left by a manually resolved incident.
		return false, s.ResetAuthRecovery(provider, accountID)
	}
	if required < 1 {
		required = 1
	}
	if required > 10 {
		required = 10
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`INSERT INTO auth_recovery_state(provider,account_id,success_count,updated_at) VALUES(?,?,1,?) ON CONFLICT(provider,account_id) DO UPDATE SET success_count=success_count+1,updated_at=excluded.updated_at`, provider, accountID, now)
	if err != nil {
		return false, err
	}
	var count int
	if err = s.db.QueryRow(`SELECT success_count FROM auth_recovery_state WHERE provider=? AND account_id=?`, provider, accountID).Scan(&count); err != nil {
		return false, err
	}
	if count < required {
		return false, nil
	}
	_, err = s.db.Exec(`DELETE FROM auth_recovery_state WHERE provider=? AND account_id=?`, provider, accountID)
	return err == nil, err
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{SnapshotDays: 90, CycleDays: 365, AlertDays: 90, BackupDays: 30, BatchSize: 1000}
}

func NormalizeRetentionPolicy(p RetentionPolicy) RetentionPolicy {
	d := DefaultRetentionPolicy()
	boundDays := func(v, fallback int) int {
		if v <= 0 {
			return fallback
		}
		if v > 3650 {
			return 3650
		}
		return v
	}
	p.SnapshotDays = boundDays(p.SnapshotDays, d.SnapshotDays)
	p.CycleDays = boundDays(p.CycleDays, d.CycleDays)
	p.AlertDays = boundDays(p.AlertDays, d.AlertDays)
	p.BackupDays = boundDays(p.BackupDays, d.BackupDays)
	if p.BatchSize <= 0 {
		p.BatchSize = d.BatchSize
	}
	if p.BatchSize > 5000 {
		p.BatchSize = 5000
	}
	return p
}

func (s *Store) RetentionPolicy() RetentionPolicy {
	p := DefaultRetentionPolicy()
	if raw, err := s.GetSetting(SettingRetentionPolicy); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return NormalizeRetentionPolicy(p)
}

type RetentionReport struct {
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	DeletedRows map[string]int `json:"deleted_rows"`
}

type retentionSnapshotSpec struct{ parent, child, captured string }
type retentionSimpleSpec struct{ table, timestamp, extra string }

var retentionSnapshots = []retentionSnapshotSpec{
	{"anthropic_snapshots", "anthropic_quota_values", "captured_at"},
	{"copilot_snapshots", "copilot_quota_values", "captured_at"},
	{"codex_snapshots", "codex_quota_values", "captured_at"},
	{"antigravity_snapshots", "antigravity_model_values", "captured_at"},
	{"minimax_snapshots", "minimax_model_values", "captured_at"},
	{"gemini_snapshots", "gemini_quota_values", "captured_at"},
	{"cursor_snapshots", "cursor_quota_values", "captured_at"},
	{"grok_snapshots", "grok_quota_values", "captured_at"},
	{"kimi_snapshots", "kimi_quota_values", "captured_at"},
	{"opencode_snapshots", "opencode_quota_values", "captured_at"},
}

var retentionSimpleSnapshots = []retentionSimpleSpec{
	{"quota_snapshots", "captured_at", ""}, {"zai_snapshots", "captured_at", ""},
	{"zai_hourly_usage", "fetched_at", ""}, {"moonshot_snapshots", "captured_at", ""},
	{"deepseek_snapshots", "captured_at", ""}, {"openrouter_snapshots", "captured_at", ""},
}

var retentionCycles = []retentionSimpleSpec{
	{"sessions", "started_at", "ended_at IS NOT NULL"},
	{"reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"zai_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"anthropic_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"copilot_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"codex_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"antigravity_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"minimax_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"gemini_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"cursor_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"grok_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"kimi_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"opencode_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"moonshot_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"}, {"deepseek_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
	{"openrouter_reset_cycles", "cycle_start", "cycle_end IS NOT NULL"},
}

func addDeleted(report *RetentionReport, name string, result sql.Result) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	report.DeletedRows[name] += int(n)
	return nil
}

// RunRetention removes at most BatchSize rows from each telemetry family. All
// identifiers come from the static tables above; user input is only bound data.
func (s *Store) RunRetention(policy RetentionPolicy, now time.Time) (RetentionReport, error) {
	policy = NormalizeRetentionPolicy(policy)
	report := RetentionReport{StartedAt: now.UTC(), DeletedRows: map[string]int{}}
	snapshotCutoff := now.Add(-time.Duration(policy.SnapshotDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	cycleCutoff := now.Add(-time.Duration(policy.CycleDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	alertCutoff := now.Add(-time.Duration(policy.AlertDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, spec := range retentionSnapshots {
		ids := fmt.Sprintf(`SELECT id FROM %s WHERE %s < ? ORDER BY id LIMIT ?`, spec.parent, spec.captured)
		res, execErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE snapshot_id IN (%s)`, spec.child, ids), snapshotCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", spec.child, execErr)
		}
		if err = addDeleted(&report, spec.child, res); err != nil {
			return report, err
		}
		res, execErr = tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id IN (%s)`, spec.parent, ids), snapshotCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", spec.parent, execErr)
		}
		if err = addDeleted(&report, spec.parent, res); err != nil {
			return report, err
		}
	}
	for _, spec := range retentionSimpleSnapshots {
		res, execErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? ORDER BY id LIMIT ?)`, spec.table, spec.table, spec.timestamp), snapshotCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", spec.table, execErr)
		}
		if err = addDeleted(&report, spec.table, res); err != nil {
			return report, err
		}
	}
	for _, spec := range retentionCycles {
		res, execErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? AND %s ORDER BY id LIMIT ?)`, spec.table, spec.table, spec.timestamp, spec.extra), cycleCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", spec.table, execErr)
		}
		if err = addDeleted(&report, spec.table, res); err != nil {
			return report, err
		}
	}
	for _, spec := range []retentionSimpleSpec{{"system_alerts", "created_at", ""}, {"notification_log", "sent_at", ""}} {
		res, execErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s < ? ORDER BY id LIMIT ?)`, spec.table, spec.table, spec.timestamp), alertCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", spec.table, execErr)
		}
		if err = addDeleted(&report, spec.table, res); err != nil {
			return report, err
		}
	}
	for _, table := range []string{"auth_recovery_state", "auth_failure_state"} {
		res, execErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE updated_at < ? ORDER BY updated_at LIMIT ?)`, table, table), alertCutoff, policy.BatchSize)
		if execErr != nil {
			return report, fmt.Errorf("prune %s: %w", table, execErr)
		}
		if err = addDeleted(&report, table, res); err != nil {
			return report, err
		}
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	report.CompletedAt = time.Now().UTC()
	return report, nil
}

type MaintenanceStatus struct {
	DatabaseBytes   int64  `json:"database_bytes"`
	WALBytes        int64  `json:"wal_bytes"`
	PageSize        int64  `json:"page_size"`
	PageCount       int64  `json:"page_count"`
	FreePages       int64  `json:"free_pages"`
	EstimatedGrowth string `json:"estimated_growth"`
	BackupDirectory string `json:"-"`
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) MaintenanceStatus() (MaintenanceStatus, error) {
	var out MaintenanceStatus
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&out.PageSize); err != nil {
		return out, err
	}
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&out.PageCount); err != nil {
		return out, err
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&out.FreePages); err != nil {
		return out, err
	}
	out.DatabaseBytes = out.PageSize * out.PageCount
	if s.dbPath != ":memory:" && !strings.Contains(strings.ToLower(s.dbPath), "mode=memory") {
		if size := fileSize(s.dbPath); size > out.DatabaseBytes {
			out.DatabaseBytes = size
		}
		out.WALBytes = fileSize(s.dbPath + "-wal")
	}
	out.EstimatedGrowth = "按 120 秒轮询时，每个活跃 OpenCode 账号约增长 0.5-1.4 MiB/天；实际增长取决于服务商和额度项数量。"
	out.BackupDirectory = s.backupDir()
	return out, nil
}

func (s *Store) Checkpoint() error { _, err := s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); return err }

// CheckHealth performs a bounded SQLite consistency and core-schema check for
// post-update verification. It does not scan application data tables.
func (s *Store) CheckHealth() error {
	var integrity string
	if err := s.db.QueryRow(`PRAGMA quick_check(1)`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite quick check failed")
	}
	var coreTables int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('schema_version','settings','system_alerts')`).Scan(&coreTables); err != nil {
		return err
	}
	if coreTables != 3 || s.SchemaVersion() <= 0 {
		return fmt.Errorf("managed schema is incomplete")
	}
	return nil
}

func (s *Store) SchemaVersion() int {
	var version sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || !version.Valid {
		return 0
	}
	return int(version.Int64)
}

type BackupInfo struct {
	Name      string    `json:"name"`
	Path      string    `json:"-"`
	Reason    string    `json:"reason"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Verified  bool      `json:"verified"`
}

func (s *Store) backupDir() string {
	if s.dbPath == ":memory:" || strings.Contains(strings.ToLower(s.dbPath), "mode=memory") {
		return ""
	}
	return filepath.Join(filepath.Dir(s.dbPath), "backups")
}

func sanitizeBackupReason(reason string) string {
	switch reason {
	case BackupReasonRetention, BackupReasonRestore:
		return reason
	default:
		return BackupReasonManual
	}
}

func (s *Store) CreateBackup(reason string) (BackupInfo, error) {
	var out BackupInfo
	dir := s.backupDir()
	if dir == "" {
		return out, errors.New("backups require a file-backed database")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return out, err
	}
	reason = sanitizeBackupReason(reason)
	stamp := time.Now().UTC()
	name := fmt.Sprintf("onwatch-%s-%s.db", stamp.Format("20060102T150405.000000000Z"), reason)
	path := filepath.Join(dir, name)
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return out, fmt.Errorf("create SQLite backup: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = os.Remove(path)
		return out, err
	}
	out = BackupInfo{Name: name, Path: path, Reason: reason, SizeBytes: fileSize(path), CreatedAt: stamp}
	if err := verifySQLiteFile(path); err != nil {
		_ = os.Remove(path)
		return BackupInfo{}, err
	}
	out.Verified = true
	return out, nil
}

func (s *Store) resolveBackup(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(strings.ToLower(name), ".db") {
		return "", errors.New("invalid backup name")
	}
	dir := s.backupDir()
	if dir == "" {
		return "", errors.New("backups require a file-backed database")
	}
	path := filepath.Join(dir, name)
	if filepath.Dir(path) != filepath.Clean(dir) {
		return "", errors.New("invalid backup path")
	}
	return path, nil
}

func verifySQLiteFile(path string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check failed")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='settings'`).Scan(&count); err != nil || count != 1 {
		return errors.New("backup is not an onWatch database")
	}
	return nil
}

func (s *Store) VerifyBackup(name string) error {
	path, err := s.resolveBackup(name)
	if err != nil {
		return err
	}
	return verifySQLiteFile(path)
}

func (s *Store) OpenBackup(name string) (*os.File, int64, error) {
	path, err := s.resolveBackup(name)
	if err != nil {
		return nil, 0, err
	}
	if err = verifySQLiteFile(path); err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	return file, fileSize(path), nil
}

func backupReasonFromName(name string) string {
	for _, reason := range []string{BackupReasonRetention, BackupReasonRestore, BackupReasonManual} {
		if strings.HasSuffix(strings.TrimSuffix(name, ".db"), "-"+reason) {
			return reason
		}
	}
	return BackupReasonManual
}

func (s *Store) ListBackups() ([]BackupInfo, error) {
	dir := s.backupDir()
	if dir == "" {
		return []BackupInfo{}, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "onwatch-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Name: entry.Name(), Reason: backupReasonFromName(entry.Name()), SizeBytes: info.Size(), CreatedAt: info.ModTime().UTC(), Verified: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) DeleteBackup(name string) error {
	marker := s.dbPath + pendingRestoreSuffix
	if payload, readErr := os.ReadFile(marker); readErr == nil {
		var request struct {
			BackupName string `json:"backup_name"`
		}
		if json.Unmarshal(payload, &request) != nil {
			return errors.New("pending restore request is invalid")
		}
		if request.BackupName == name {
			return ErrBackupStagedForRestore
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	path, err := s.resolveBackup(name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Store) PruneBackups(days int, now time.Time) (int, error) {
	policy := NormalizeRetentionPolicy(RetentionPolicy{BackupDays: days})
	items, err := s.ListBackups()
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-time.Duration(policy.BackupDays) * 24 * time.Hour)
	deleted := 0
	for _, item := range items {
		if !item.CreatedAt.Before(cutoff) {
			continue
		}
		if err := s.DeleteBackup(item.Name); err != nil {
			if errors.Is(err, ErrBackupStagedForRestore) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (s *Store) StageRestore(name string) error {
	path, err := s.resolveBackup(name)
	if err != nil {
		return err
	}
	if err = verifySQLiteFile(path); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"backup_name": name, "requested_at": time.Now().UTC().Format(time.RFC3339Nano)})
	marker := s.dbPath + pendingRestoreSuffix
	tmp, err := os.CreateTemp(filepath.Dir(marker), ".onwatch-restore-request-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(payload)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Windows does not replace an existing destination with os.Rename. The
	// fully-synced temp file remains available if removing the old marker fails.
	if removeErr := os.Remove(marker); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return os.Rename(tmpPath, marker)
}

type RestoreResult struct {
	Applied      bool   `json:"applied"`
	RollbackPath string `json:"rollback_path,omitempty"`
}

func copyFileSecure(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func createConsistentSQLiteCopy(src, dst string) error {
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		return err
	}
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0600); err != nil {
		_ = os.Remove(dst)
		return err
	}
	if err := verifySQLiteFile(dst); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// ApplyPendingRestore is intentionally called before Store.New so no SQLite
// connection can hold WAL state while the database file is replaced.
func ApplyPendingRestore(dbPath string) (RestoreResult, error) {
	var out RestoreResult
	marker := dbPath + pendingRestoreSuffix
	payload, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var request struct {
		BackupName string `json:"backup_name"`
	}
	if json.Unmarshal(payload, &request) != nil || filepath.Base(request.BackupName) != request.BackupName || strings.ContainsAny(request.BackupName, `/\`) {
		return out, errors.New("invalid pending restore request")
	}
	source := filepath.Join(filepath.Dir(dbPath), "backups", request.BackupName)
	if err = verifySQLiteFile(source); err != nil {
		return out, err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	rollback := filepath.Join(filepath.Dir(dbPath), "backups", "onwatch-"+stamp+"-"+BackupReasonRestore+".db")
	if err = os.MkdirAll(filepath.Dir(rollback), 0700); err != nil {
		return out, err
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		// VACUUM INTO includes committed pages still present in a crash-recovery
		// WAL. Copying only the main .db file could silently lose those pages.
		if err = createConsistentSQLiteCopy(dbPath, rollback); err != nil {
			return out, err
		}
	}
	tmp := dbPath + ".restore.tmp"
	_ = os.Remove(tmp)
	if err = copyFileSecure(source, tmp); err != nil {
		return out, err
	}
	if err = verifySQLiteFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return out, err
	}
	old := dbPath + ".restore.old"
	_ = os.Remove(old)
	if _, statErr := os.Stat(dbPath); statErr == nil {
		if err = os.Rename(dbPath, old); err != nil {
			_ = os.Remove(tmp)
			return out, err
		}
	}
	if err = os.Rename(tmp, dbPath); err != nil {
		_ = os.Rename(old, dbPath)
		return out, err
	}
	_ = os.Remove(old)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	_ = os.Remove(marker)
	out.Applied, out.RollbackPath = true, rollback
	return out, nil
}
