package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
)

type OpenCodeResetCycle struct {
	ID              int64
	AccountID       int64
	QuotaName       string
	CycleStart      time.Time
	CycleEnd        *time.Time
	ResetsAt        *time.Time
	PeakUtilization float64
	TotalDelta      float64
}

type OpenCodeLatestQuota struct {
	AccountID   int64
	Name        string
	Used        float64
	Limit       float64
	Utilization float64
	Format      string
	ResetsAt    *time.Time
	CapturedAt  time.Time
	AccountType string
	PlanName    string
}

func (s *Store) defaultOpenCodeAccountID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT oa.account_id FROM opencode_accounts oa JOIN provider_accounts pa ON pa.id=oa.account_id WHERE pa.deleted_at IS NULL ORDER BY oa.account_id LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	account, err := s.CreateOrRestoreProviderAccount("opencode", "default")
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`INSERT OR IGNORE INTO opencode_accounts(account_id, workspace_id, enabled, auth_status, credential_version, created_at, updated_at) VALUES (?, ?, 0, 'disabled', 0, ?, ?)`, account.ID, fmt.Sprintf("default-%d", account.ID), now, now)
	return account.ID, err
}

func (s *Store) DefaultOpenCodeAccountID() (int64, error) {
	return s.defaultOpenCodeAccountID()
}

func (s *Store) InsertOpenCodeSnapshot(snapshot *api.OpenCodeSnapshot) (int64, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return 0, err
	}
	return s.InsertOpenCodeSnapshotForAccount(id, snapshot)
}

func (s *Store) InsertOpenCodeSnapshotForAccount(accountID int64, snapshot *api.OpenCodeSnapshot) (int64, error) {
	if snapshot == nil {
		return 0, fmt.Errorf("OpenCode snapshot is nil")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO opencode_snapshots (account_id, captured_at, raw_json, account_type, plan_name, quota_count) VALUES (?, ?, ?, ?, ?, ?)`, accountID, snapshot.CapturedAt.Format(time.RFC3339Nano), snapshot.RawJSON, string(snapshot.AccountType), snapshot.PlanName, len(snapshot.Quotas))
	if err != nil {
		return 0, fmt.Errorf("failed to insert opencode snapshot: %w", err)
	}
	snapshotID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, q := range snapshot.Quotas {
		var resetsAt any
		if q.ResetsAt != nil {
			resetsAt = q.ResetsAt.Format(time.RFC3339Nano)
		}
		if _, err := tx.Exec(`INSERT INTO opencode_quota_values (account_id, snapshot_id, quota_name, used, limit_value, utilization, format, resets_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, accountID, snapshotID, q.Name, q.Used, q.Limit, q.Utilization, string(q.Format), resetsAt); err != nil {
			return 0, fmt.Errorf("failed to insert quota value %s: %w", q.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return snapshotID, nil
}

func (s *Store) QueryLatestOpenCode() (*api.OpenCodeSnapshot, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryLatestOpenCodeForAccount(id)
}

func (s *Store) QueryLatestOpenCodeForAccount(accountID int64) (*api.OpenCodeSnapshot, error) {
	var snap api.OpenCodeSnapshot
	var capturedAt, accountType, planName string
	err := s.db.QueryRow(`SELECT id, captured_at, account_type, plan_name, quota_count FROM opencode_snapshots WHERE account_id=? ORDER BY captured_at DESC, id DESC LIMIT 1`, accountID).Scan(&snap.ID, &capturedAt, &accountType, &planName, new(int))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query latest opencode: %w", err)
	}
	snap.CapturedAt, _ = time.Parse(time.RFC3339Nano, capturedAt)
	snap.AccountType, snap.PlanName = api.OpenCodeAccountType(accountType), planName
	if err := s.loadOpenCodeQuotas(accountID, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *Store) loadOpenCodeQuotas(accountID int64, snap *api.OpenCodeSnapshot) error {
	rows, err := s.db.Query(`SELECT quota_name, used, limit_value, utilization, format, resets_at FROM opencode_quota_values WHERE account_id=? AND snapshot_id=? ORDER BY quota_name`, accountID, snap.ID)
	if err != nil {
		return fmt.Errorf("failed to query quota values: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var q api.OpenCodeQuota
		var format string
		var resetsAt sql.NullString
		if err := rows.Scan(&q.Name, &q.Used, &q.Limit, &q.Utilization, &format, &resetsAt); err != nil {
			return err
		}
		q.Format = api.OpenCodeQuotaFormat(format)
		if resetsAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, resetsAt.String)
			q.ResetsAt = &t
		}
		snap.Quotas = append(snap.Quotas, q)
	}
	return rows.Err()
}

func (s *Store) QueryOpenCodeRange(start, end time.Time, limit ...int) ([]*api.OpenCodeSnapshot, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeRangeForAccount(id, start, end, limit...)
}

func (s *Store) QueryOpenCodeRangeForAccount(accountID int64, start, end time.Time, limit ...int) ([]*api.OpenCodeSnapshot, error) {
	query := `SELECT id, captured_at, account_type, plan_name, quota_count FROM opencode_snapshots WHERE account_id=? AND captured_at BETWEEN ? AND ? ORDER BY captured_at ASC, id ASC`
	args := []any{accountID, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano)}
	if len(limit) > 0 && limit[0] > 0 {
		query = `SELECT id, captured_at, account_type, plan_name, quota_count FROM (SELECT id, captured_at, account_type, plan_name, quota_count FROM opencode_snapshots WHERE account_id=? AND captured_at BETWEEN ? AND ? ORDER BY captured_at DESC, id DESC LIMIT ?) recent ORDER BY captured_at ASC, id ASC`
		args = append(args, limit[0])
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.OpenCodeSnapshot
	for rows.Next() {
		var snap api.OpenCodeSnapshot
		var captured, typ, plan string
		if err := rows.Scan(&snap.ID, &captured, &typ, &plan, new(int)); err != nil {
			return nil, err
		}
		snap.CapturedAt, _ = time.Parse(time.RFC3339Nano, captured)
		snap.AccountType, snap.PlanName = api.OpenCodeAccountType(typ), plan
		out = append(out, &snap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, snap := range out {
		if err := s.loadOpenCodeQuotas(accountID, snap); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) CreateOpenCodeCycle(quotaName string, cycleStart time.Time, resetsAt *time.Time) (int64, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return 0, err
	}
	return s.CreateOpenCodeCycleForAccount(id, quotaName, cycleStart, resetsAt)
}

func (s *Store) CreateOpenCodeCycleForAccount(accountID int64, quotaName string, cycleStart time.Time, resetsAt *time.Time) (int64, error) {
	var reset any
	if resetsAt != nil {
		reset = resetsAt.Format(time.RFC3339Nano)
	}
	r, err := s.db.Exec(`INSERT INTO opencode_reset_cycles(account_id,quota_name,cycle_start,resets_at) VALUES(?,?,?,?)`, accountID, quotaName, cycleStart.Format(time.RFC3339Nano), reset)
	if err != nil {
		return 0, fmt.Errorf("failed to create opencode cycle: %w", err)
	}
	return r.LastInsertId()
}

func (s *Store) CloseOpenCodeCycle(quotaName string, cycleEnd time.Time, peak, delta float64) error {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return err
	}
	return s.CloseOpenCodeCycleForAccount(id, quotaName, cycleEnd, peak, delta)
}

func (s *Store) CloseOpenCodeCycleForAccount(accountID int64, quotaName string, cycleEnd time.Time, peak, delta float64) error {
	_, err := s.db.Exec(`UPDATE opencode_reset_cycles SET cycle_end=?,peak_utilization=?,total_delta=? WHERE account_id=? AND quota_name=? AND cycle_end IS NULL`, cycleEnd.Format(time.RFC3339Nano), peak, delta, accountID, quotaName)
	return err
}

func (s *Store) UpdateOpenCodeCycle(quotaName string, peak, delta float64) error {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return err
	}
	return s.UpdateOpenCodeCycleForAccount(id, quotaName, peak, delta)
}

func (s *Store) UpdateOpenCodeCycleForAccount(accountID int64, quotaName string, peak, delta float64) error {
	_, err := s.db.Exec(`UPDATE opencode_reset_cycles SET peak_utilization=?,total_delta=? WHERE account_id=? AND quota_name=? AND cycle_end IS NULL`, peak, delta, accountID, quotaName)
	return err
}

func scanOpenCodeCycle(scanner interface{ Scan(...any) error }) (*OpenCodeResetCycle, error) {
	var c OpenCodeResetCycle
	var start string
	var end, reset sql.NullString
	if err := scanner.Scan(&c.ID, &c.AccountID, &c.QuotaName, &start, &end, &reset, &c.PeakUtilization, &c.TotalDelta); err != nil {
		return nil, err
	}
	c.CycleStart, _ = time.Parse(time.RFC3339Nano, start)
	if end.Valid {
		t, _ := time.Parse(time.RFC3339Nano, end.String)
		c.CycleEnd = &t
	}
	if reset.Valid {
		t, _ := time.Parse(time.RFC3339Nano, reset.String)
		c.ResetsAt = &t
	}
	return &c, nil
}

const openCodeCycleSelect = `SELECT id,account_id,quota_name,cycle_start,cycle_end,resets_at,peak_utilization,total_delta FROM opencode_reset_cycles`

func (s *Store) QueryActiveOpenCodeCycle(quotaName string) (*OpenCodeResetCycle, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryActiveOpenCodeCycleForAccount(id, quotaName)
}

func (s *Store) QueryActiveOpenCodeCycleForAccount(accountID int64, quotaName string) (*OpenCodeResetCycle, error) {
	c, err := scanOpenCodeCycle(s.db.QueryRow(openCodeCycleSelect+` WHERE account_id=? AND quota_name=? AND cycle_end IS NULL`, accountID, quotaName))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *Store) QueryOpenCodeCycleHistory(quotaName string, limit ...int) ([]*OpenCodeResetCycle, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeCycleHistoryForAccount(id, quotaName, limit...)
}

func (s *Store) QueryOpenCodeCycleHistoryForAccount(accountID int64, quotaName string, limit ...int) ([]*OpenCodeResetCycle, error) {
	q := openCodeCycleSelect + ` WHERE account_id=? AND quota_name=? AND cycle_end IS NOT NULL ORDER BY cycle_start DESC`
	args := []any{accountID, quotaName}
	if len(limit) > 0 && limit[0] > 0 {
		q += ` LIMIT ?`
		args = append(args, limit[0])
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpenCodeResetCycle
	for rows.Next() {
		c, err := scanOpenCodeCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) QueryOpenCodeCyclesSince(quotaName string, since time.Time) ([]*OpenCodeResetCycle, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeCyclesSinceForAccount(id, quotaName, since)
}

func (s *Store) QueryOpenCodeCyclesSinceForAccount(accountID int64, quotaName string, since time.Time) ([]*OpenCodeResetCycle, error) {
	rows, err := s.db.Query(openCodeCycleSelect+` WHERE account_id=? AND quota_name=? AND cycle_end IS NOT NULL AND cycle_start>=? ORDER BY cycle_start DESC LIMIT 200`, accountID, quotaName, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OpenCodeResetCycle
	for rows.Next() {
		c, err := scanOpenCodeCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) QueryOpenCodeUtilizationSeries(quotaName string, since time.Time) ([]UtilizationPoint, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeUtilizationSeriesForAccount(id, quotaName, since, 500)
}

func (s *Store) QueryOpenCodeUtilizationSeriesForAccount(accountID int64, quotaName string, since time.Time, limit int) ([]UtilizationPoint, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT captured_at,utilization FROM (SELECT s.captured_at,q.utilization FROM opencode_quota_values q JOIN opencode_snapshots s ON s.id=q.snapshot_id AND s.account_id=q.account_id WHERE q.account_id=? AND q.quota_name=? AND s.captured_at>=? ORDER BY s.captured_at DESC,s.id DESC LIMIT ?) recent ORDER BY captured_at ASC`, accountID, quotaName, since.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UtilizationPoint
	for rows.Next() {
		var ts string
		var utilization float64
		if err := rows.Scan(&ts, &utilization); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		out = append(out, UtilizationPoint{CapturedAt: t, Utilization: utilization})
	}
	return out, rows.Err()
}

// OpenCodeAggregateHistoryPoint is one quota's average utilization in a
// bounded time bucket. Each account contributes at most its latest sample in
// the bucket, preventing faster pollers from receiving extra weight.
type OpenCodeAggregateHistoryPoint struct {
	CapturedAt         time.Time
	QuotaName          string
	AverageUtilization float64
	SampleCount        int
}

// QueryOpenCodeAggregateHistory returns a downsampled all-account trend. It
// excludes disabled and soft-deleted accounts and bounds the number of time
// buckets independently of the number of configured accounts.
func (s *Store) QueryOpenCodeAggregateHistory(start, end time.Time, bucketSize time.Duration, limit int) ([]OpenCodeAggregateHistoryPoint, error) {
	if bucketSize < time.Minute {
		bucketSize = time.Minute
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	bucketSeconds := int64(bucketSize / time.Second)
	rows, err := s.db.Query(`
		WITH raw AS (
			SELECT q.account_id, q.quota_name, q.utilization, s.captured_at, s.id AS snapshot_id,
			       CAST(strftime('%s', s.captured_at) AS INTEGER) / ? AS bucket
			FROM opencode_quota_values q
			JOIN opencode_snapshots s ON s.id=q.snapshot_id AND s.account_id=q.account_id
			JOIN opencode_accounts oa ON oa.account_id=q.account_id
			JOIN provider_accounts pa ON pa.id=q.account_id
			WHERE s.captured_at BETWEEN ? AND ? AND oa.enabled=1 AND pa.deleted_at IS NULL
		), ranked AS (
			SELECT account_id, quota_name, utilization, captured_at, bucket,
			       ROW_NUMBER() OVER (PARTITION BY account_id, quota_name, bucket ORDER BY captured_at DESC, snapshot_id DESC) AS sample_rank
			FROM raw
		), recent_buckets AS (
			SELECT DISTINCT bucket FROM ranked WHERE sample_rank=1 ORDER BY bucket DESC LIMIT ?
		)
		SELECT r.bucket, r.quota_name, AVG(r.utilization), COUNT(*)
		FROM ranked r JOIN recent_buckets b ON b.bucket=r.bucket
		WHERE r.sample_rank=1
		GROUP BY r.bucket, r.quota_name
		ORDER BY r.bucket ASC, r.quota_name ASC`,
		bucketSeconds, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query aggregate opencode history: %w", err)
	}
	defer rows.Close()

	out := make([]OpenCodeAggregateHistoryPoint, 0)
	for rows.Next() {
		var bucket int64
		var point OpenCodeAggregateHistoryPoint
		if err := rows.Scan(&bucket, &point.QuotaName, &point.AverageUtilization, &point.SampleCount); err != nil {
			return nil, err
		}
		point.CapturedAt = time.Unix(bucket*bucketSeconds, 0).UTC()
		out = append(out, point)
	}
	return out, rows.Err()
}

func (s *Store) QueryOpenCodeLatestPerQuota() ([]OpenCodeLatestQuota, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeLatestPerQuotaForAccount(id)
}

func (s *Store) QueryOpenCodeLatestPerQuotaForAccount(accountID int64) ([]OpenCodeLatestQuota, error) {
	rows, err := s.db.Query(`SELECT q.quota_name,q.used,q.limit_value,q.utilization,q.format,q.resets_at,s.captured_at,s.account_type,s.plan_name FROM opencode_quota_values q JOIN opencode_snapshots s ON s.id=q.snapshot_id AND s.account_id=q.account_id WHERE q.account_id=? AND s.id=(SELECT id FROM opencode_snapshots WHERE account_id=? ORDER BY captured_at DESC,id DESC LIMIT 1) ORDER BY q.quota_name`, accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenCodeLatestQuota
	for rows.Next() {
		q := OpenCodeLatestQuota{AccountID: accountID}
		var reset sql.NullString
		var captured string
		if err := rows.Scan(&q.Name, &q.Used, &q.Limit, &q.Utilization, &q.Format, &reset, &captured, &q.AccountType, &q.PlanName); err != nil {
			return nil, err
		}
		q.CapturedAt, _ = time.Parse(time.RFC3339Nano, captured)
		if reset.Valid {
			t, _ := time.Parse(time.RFC3339Nano, reset.String)
			q.ResetsAt = &t
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) QueryAllOpenCodeQuotaNames() ([]string, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryAllOpenCodeQuotaNamesForAccount(id)
}

func (s *Store) QueryAllOpenCodeQuotaNamesForAccount(accountID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT quota_name FROM opencode_reset_cycles WHERE account_id=? ORDER BY quota_name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) QueryOpenCodeCycleOverview(groupBy string, limit int) ([]CycleOverviewRow, error) {
	id, err := s.defaultOpenCodeAccountID()
	if err != nil {
		return nil, err
	}
	return s.QueryOpenCodeCycleOverviewForAccount(id, groupBy, limit)
}

func (s *Store) QueryOpenCodeCycleOverviewForAccount(accountID int64, groupBy string, limit int) ([]CycleOverviewRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var cycles []*OpenCodeResetCycle
	if active, err := s.QueryActiveOpenCodeCycleForAccount(accountID, groupBy); err != nil {
		return nil, err
	} else if active != nil {
		cycles = append(cycles, active)
		limit--
	}
	history, err := s.QueryOpenCodeCycleHistoryForAccount(accountID, groupBy, limit)
	if err != nil {
		return nil, err
	}
	cycles = append(cycles, history...)
	var out []CycleOverviewRow
	for _, c := range cycles {
		row := CycleOverviewRow{CycleID: c.ID, QuotaType: c.QuotaName, CycleStart: c.CycleStart, CycleEnd: c.CycleEnd, PeakValue: c.PeakUtilization, TotalDelta: c.TotalDelta}
		end := time.Now().Add(time.Minute)
		if c.CycleEnd != nil {
			end = *c.CycleEnd
		}
		var snapshotID int64
		var captured string
		err := s.db.QueryRow(`SELECT s.id,s.captured_at FROM opencode_snapshots s JOIN opencode_quota_values q ON q.snapshot_id=s.id AND q.account_id=s.account_id WHERE s.account_id=? AND q.quota_name=? AND s.captured_at>=? AND s.captured_at<? ORDER BY q.utilization DESC LIMIT 1`, accountID, groupBy, c.CycleStart.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)).Scan(&snapshotID, &captured)
		if err == sql.ErrNoRows {
			out = append(out, row)
			continue
		}
		if err != nil {
			return nil, err
		}
		row.PeakTime, _ = time.Parse(time.RFC3339Nano, captured)
		rows, err := s.db.Query(`SELECT quota_name,utilization,used,limit_value FROM opencode_quota_values WHERE account_id=? AND snapshot_id=? ORDER BY quota_name`, accountID, snapshotID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var entry CrossQuotaEntry
			if err := rows.Scan(&entry.Name, &entry.Percent, &entry.Value, &entry.Limit); err != nil {
				rows.Close()
				return nil, err
			}
			row.CrossQuotas = append(row.CrossQuotas, entry)
		}
		rows.Close()
		out = append(out, row)
	}
	return out, nil
}

type OpenCodeAccountSummary struct {
	Account  OpenCodeAccount       `json:"account"`
	Snapshot *api.OpenCodeSnapshot `json:"snapshot,omitempty"`
}

func (s *Store) QueryOpenCodeAccountSummaries() ([]OpenCodeAccountSummary, error) {
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil {
		return nil, err
	}
	out := make([]OpenCodeAccountSummary, 0, len(accounts))
	for _, account := range accounts {
		snapshot, err := s.QueryLatestOpenCodeForAccount(account.AccountID)
		if err != nil {
			return nil, err
		}
		out = append(out, OpenCodeAccountSummary{Account: account, Snapshot: snapshot})
	}
	return out, nil
}
