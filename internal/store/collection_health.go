package store

import (
	"database/sql"
	"fmt"
	"time"
)

type CollectionHealth struct {
	Provider            string     `json:"provider"`
	AccountID           int64      `json:"account_id,omitempty"`
	AccountName         string     `json:"account_name,omitempty"`
	Configured          bool       `json:"configured"`
	Enabled             bool       `json:"enabled"`
	Status              string     `json:"status"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt         *time.Time `json:"last_error_at,omitempty"`
	ErrorCode           string     `json:"error_code,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	NextRetryAt         *time.Time `json:"next_retry_at,omitempty"`
}

func parseHealthTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value.String); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}

func freshnessStatus(last *time.Time, interval time.Duration, now time.Time) string {
	if last == nil {
		return "pending"
	}
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	if now.Sub(*last) > 2*interval+time.Minute {
		return "stale"
	}
	return "healthy"
}

func (s *Store) latestTimestamp(table string) (*time.Time, error) {
	// table is selected exclusively from the static list in QueryCollectionHealth.
	var value sql.NullString
	if err := s.db.QueryRow(fmt.Sprintf(`SELECT MAX(captured_at) FROM %s`, table)).Scan(&value); err != nil {
		return nil, err
	}
	return parseHealthTime(value), nil
}

func (s *Store) QueryCollectionHealth(interval time.Duration, now time.Time) ([]CollectionHealth, error) {
	static := []struct{ provider, table string }{
		{"synthetic", "quota_snapshots"}, {"zai", "zai_snapshots"}, {"anthropic", "anthropic_snapshots"},
		{"copilot", "copilot_snapshots"}, {"antigravity", "antigravity_snapshots"}, {"openrouter", "openrouter_snapshots"},
		{"moonshot", "moonshot_snapshots"}, {"deepseek", "deepseek_snapshots"}, {"gemini", "gemini_snapshots"},
		{"cursor", "cursor_snapshots"}, {"grok", "grok_snapshots"}, {"kimi", "kimi_snapshots"},
	}
	out := make([]CollectionHealth, 0, len(static)+8)
	for _, item := range static {
		last, err := s.latestTimestamp(item.table)
		if err != nil {
			return nil, err
		}
		out = append(out, CollectionHealth{Provider: item.provider, Enabled: true, Status: freshnessStatus(last, interval, now), LastSuccessAt: last})
	}
	for _, item := range []struct{ provider, table string }{{"codex", "codex_snapshots"}, {"minimax", "minimax_snapshots"}} {
		rows, err := s.db.Query(fmt.Sprintf(`SELECT pa.id,pa.name,MAX(s.captured_at) FROM provider_accounts pa LEFT JOIN %s s ON s.account_id=pa.id WHERE pa.provider=? AND pa.deleted_at IS NULL GROUP BY pa.id,pa.name ORDER BY pa.id`, item.table), item.provider)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name string
			var raw sql.NullString
			if err := rows.Scan(&id, &name, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			last := parseHealthTime(raw)
			out = append(out, CollectionHealth{Provider: item.provider, AccountID: id, AccountName: name, Enabled: true, Status: freshnessStatus(last, interval, now), LastSuccessAt: last})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		status := account.AuthStatus
		if status == OpenCodeAuthValid {
			status = freshnessStatus(account.LastSuccessAt, interval, now)
		}
		out = append(out, CollectionHealth{Provider: "opencode", AccountID: account.AccountID, AccountName: account.Name, Enabled: account.Enabled, Status: status, LastAttemptAt: account.LastPollAt, LastSuccessAt: account.LastSuccessAt, LastErrorAt: account.LastErrorAt, ErrorCode: account.LastErrorCode, ConsecutiveFailures: account.ConsecutiveFailures, NextRetryAt: account.NextPollAt})
	}
	return out, nil
}
