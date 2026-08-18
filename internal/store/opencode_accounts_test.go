package store

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	_ "modernc.org/sqlite"
)

func setOpenCodeTestKey(t *testing.T) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("ONWATCH_CREDENTIAL_KEY", key)
}

func TestOpenCodeAccountsEncryptCredentialsAndScrubOnDelete(t *testing.T) {
	setOpenCodeTestKey(t)
	dbPath := filepath.Join(t.TempDir(), "onwatch.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	account, err := s.CreateOpenCodeAccount("Work", "ws-1", "super-secret-cookie", true)
	if err != nil {
		t.Fatalf("CreateOpenCodeAccount: %v", err)
	}
	if account.AuthCookie != "" || !account.HasAuthCookie {
		t.Fatalf("account exposed secret or omitted presence flag: %+v", account)
	}

	var ciphertext string
	if err := s.db.QueryRow(`SELECT auth_cookie_ciphertext FROM opencode_accounts WHERE account_id = ?`, account.AccountID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext == "super-secret-cookie" || strings.Contains(ciphertext, "super-secret-cookie") || !strings.HasPrefix(ciphertext, "v1:") {
		t.Fatalf("cookie was not safely encrypted: %q", ciphertext)
	}
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	rawDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawDB), "super-secret-cookie") {
		t.Fatal("plaintext cookie was present in the SQLite file")
	}

	withSecret, err := s.GetOpenCodeAccount(account.AccountID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecret.AuthCookie != "super-secret-cookie" {
		t.Fatalf("decrypted cookie = %q", withSecret.AuthCookie)
	}

	if err := s.DeleteOpenCodeAccount(account.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT auth_cookie_ciphertext FROM opencode_accounts WHERE account_id = ?`, account.AccountID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != "" {
		t.Fatalf("deleted account retained ciphertext: %q", ciphertext)
	}
	provider, err := s.GetProviderAccountByID(account.AccountID)
	if err != nil || provider == nil || provider.DeletedAt == nil {
		t.Fatalf("provider account was not soft-deleted: account=%+v err=%v", provider, err)
	}

	restored, err := s.CreateOpenCodeAccount("Restored", "ws-1", "replacement-cookie", true)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AccountID != account.AccountID {
		t.Fatalf("restored account ID = %d, want stable ID %d", restored.AccountID, account.AccountID)
	}
}

func TestOpenCodeCredentialAADPreventsCrossAccountDecryption(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateOpenCodeAccount("A", "ws-a", "cookie-a", true)
	b, _ := s.CreateOpenCodeAccount("B", "ws-b", "cookie-b", true)
	var ciphertext string
	if err := s.db.QueryRow(`SELECT auth_cookie_ciphertext FROM opencode_accounts WHERE account_id=?`, a.AccountID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decryptOpenCodeCookie(b.AccountID, ciphertext); err == nil {
		t.Fatal("ciphertext decrypted under the wrong account AAD")
	}
}

func TestBootstrapLegacyOpenCodePrefersDBAndScrubsPlaintext(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy := `{"opencode":{"workspace_id":"db-workspace","auth_cookie":"db-cookie","enabled":true}}`
	if err := s.SetSetting("provider_settings", legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacyOpenCodeAccount("env-workspace", "env-cookie"); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].WorkspaceID != "db-workspace" {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	secret, err := s.GetOpenCodeAccount(accounts[0].AccountID, true)
	if err != nil || secret.AuthCookie != "db-cookie" {
		t.Fatalf("secret=%+v err=%v", secret, err)
	}
	raw, _ := s.GetSetting("provider_settings")
	if strings.Contains(raw, "db-cookie") || strings.Contains(raw, "workspace_id") || strings.Contains(raw, "auth_cookie") {
		t.Fatalf("legacy plaintext was not scrubbed: %s", raw)
	}
}

func TestBootstrapLegacyOpenCodeScrubsPlaintextFromSQLiteFiles(t *testing.T) {
	setOpenCodeTestKey(t)
	dbPath := filepath.Join(t.TempDir(), "onwatch.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyCookie := "legacy-cookie-must-not-remain-on-disk"
	legacy := `{"opencode":{"workspace_id":"db-workspace","auth_cookie":"` + legacyCookie + `","enabled":true}}`
	if err := s.SetSetting("provider_settings", legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacyOpenCodeAccount("", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		raw, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			t.Fatalf("read %s: %v", filepath.Base(path), readErr)
		}
		if strings.Contains(string(raw), legacyCookie) {
			t.Fatalf("legacy plaintext Cookie remained in %s", filepath.Base(path))
		}
	}
}

func TestBootstrapLegacyOpenCodeDoesNotOverwriteEncryptedAccount(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	existing, err := s.CreateOpenCodeAccount("Existing", "existing-workspace", "existing-cookie", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("provider_settings", `{"opencode":{"workspace_id":"legacy-workspace","auth_cookie":"legacy-cookie"}}`); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacyOpenCodeAccount("env-workspace", "env-cookie"); err != nil {
		t.Fatal(err)
	}

	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].AccountID != existing.AccountID {
		t.Fatalf("encrypted account precedence failed: accounts=%+v err=%v", accounts, err)
	}
	withSecret, err := s.GetOpenCodeAccount(existing.AccountID, true)
	if err != nil || withSecret.AuthCookie != "existing-cookie" {
		t.Fatalf("existing credential was overwritten: account=%+v err=%v", withSecret, err)
	}
	raw, err := s.GetSetting("provider_settings")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "legacy-cookie") || strings.Contains(raw, "auth_cookie") {
		t.Fatalf("stale legacy plaintext was not scrubbed: %s", raw)
	}
}

func TestBootstrapLegacyOpenCodeScrubsIncompleteLegacyCredential(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting("provider_settings", `{"opencode":{"auth_cookie":"orphaned-cookie","enabled":true}}`); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapLegacyOpenCodeAccount("", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := s.GetSetting("provider_settings")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "orphaned-cookie") || strings.Contains(raw, "auth_cookie") {
		t.Fatalf("incomplete legacy credential was not scrubbed: %s", raw)
	}
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("incomplete legacy credential created an account: accounts=%+v err=%v", accounts, err)
	}
}

func TestAuthAlertDeduplicationIsAccountScoped(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateSystemAlert("opencode", "token_refresh_failed", "reauth", "replace cookie", "error", `{"account_id":"101"}`); err != nil {
		t.Fatal(err)
	}
	active, err := s.HasActiveAlertOfTypeForAccount("opencode", "token_refresh_failed", "101")
	if err != nil || !active {
		t.Fatalf("account 101 active=%v err=%v", active, err)
	}
	active, err = s.HasActiveAlertOfTypeForAccount("opencode", "token_refresh_failed", "202")
	if err != nil || active {
		t.Fatalf("account 202 active=%v err=%v", active, err)
	}
}

func TestOpenCodeTelemetryIsAccountScoped(t *testing.T) {
	setOpenCodeTestKey(t)
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, err := s.CreateOpenCodeAccount("A", "ws-a", "cookie-a", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateOpenCodeAccount("B", "ws-b", "cookie-b", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	for _, tc := range []struct {
		accountID int64
		util      float64
	}{
		{a.AccountID, 15},
		{b.AccountID, 85},
	} {
		snapshot := &api.OpenCodeSnapshot{
			CapturedAt: now,
			PlanName:   "OpenCode Go",
			Quotas: []api.OpenCodeQuota{{
				Name: "weekly", Utilization: tc.util, Used: tc.util, Limit: 100,
				Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &reset,
			}},
		}
		if _, err := s.InsertOpenCodeSnapshotForAccount(tc.accountID, snapshot); err != nil {
			t.Fatalf("insert account %d: %v", tc.accountID, err)
		}
		if _, err := s.CreateOpenCodeCycleForAccount(tc.accountID, "weekly", now, &reset); err != nil {
			t.Fatalf("cycle account %d: %v", tc.accountID, err)
		}
		if err := s.UpdateOpenCodeCycleForAccount(tc.accountID, "weekly", tc.util, tc.util); err != nil {
			t.Fatal(err)
		}
	}

	latestA, err := s.QueryLatestOpenCodeForAccount(a.AccountID)
	if err != nil || latestA == nil || latestA.Quotas[0].Utilization != 15 {
		t.Fatalf("account A latest mixed: %+v err=%v", latestA, err)
	}
	latestB, err := s.QueryLatestOpenCodeForAccount(b.AccountID)
	if err != nil || latestB == nil || latestB.Quotas[0].Utilization != 85 {
		t.Fatalf("account B latest mixed: %+v err=%v", latestB, err)
	}
	cycleA, _ := s.QueryActiveOpenCodeCycleForAccount(a.AccountID, "weekly")
	cycleB, _ := s.QueryActiveOpenCodeCycleForAccount(b.AccountID, "weekly")
	if cycleA == nil || cycleB == nil || cycleA.PeakUtilization != 15 || cycleB.PeakUtilization != 85 {
		t.Fatalf("cycles mixed: A=%+v B=%+v", cycleA, cycleB)
	}
}

func TestOpenCodeLegacySchemaMigrationBackfillsStableAccount(t *testing.T) {
	setOpenCodeTestKey(t)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := os.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE opencode_snapshots (id INTEGER PRIMARY KEY AUTOINCREMENT, captured_at TEXT NOT NULL, raw_json TEXT NOT NULL DEFAULT '', account_type TEXT NOT NULL DEFAULT '', plan_name TEXT NOT NULL DEFAULT '', quota_count INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE opencode_quota_values (id INTEGER PRIMARY KEY AUTOINCREMENT, snapshot_id INTEGER NOT NULL, quota_name TEXT NOT NULL, used REAL NOT NULL DEFAULT 0, limit_value REAL NOT NULL DEFAULT 0, utilization REAL NOT NULL DEFAULT 0, format TEXT NOT NULL DEFAULT 'percent', resets_at TEXT);
		CREATE TABLE opencode_reset_cycles (id INTEGER PRIMARY KEY AUTOINCREMENT, quota_name TEXT NOT NULL, cycle_start TEXT NOT NULL, cycle_end TEXT, resets_at TEXT, peak_utilization REAL NOT NULL DEFAULT 0, total_delta REAL NOT NULL DEFAULT 0);
		INSERT INTO opencode_snapshots(captured_at, quota_count) VALUES ('2026-01-01T00:00:00Z', 1);
		INSERT INTO opencode_quota_values(snapshot_id, quota_name, utilization) VALUES (1, 'weekly', 42);
		INSERT INTO opencode_reset_cycles(quota_name, cycle_start, peak_utilization) VALUES ('weekly', '2026-01-01T00:00:00Z', 42);
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("legacy account ownership missing: accounts=%+v err=%v", accounts, err)
	}
	latest, err := s.QueryLatestOpenCodeForAccount(accounts[0].AccountID)
	if err != nil || latest == nil || len(latest.Quotas) != 1 || latest.Quotas[0].Utilization != 42 {
		t.Fatalf("legacy snapshot lost: latest=%+v err=%v", latest, err)
	}
	cycle, err := s.QueryActiveOpenCodeCycleForAccount(accounts[0].AccountID, "weekly")
	if err != nil || cycle == nil || cycle.PeakUtilization != 42 {
		t.Fatalf("legacy cycle lost: cycle=%+v err=%v", cycle, err)
	}
	backups, _ := filepath.Glob(dbPath + ".pre-opencode-multi-account-*.bak")
	if len(backups) != 1 {
		t.Fatalf("expected one migration backup, got %v", backups)
	}
}
