package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/config"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func TestOpenCodeAccountsCRUDNeverReturnsCookie(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := &config.Config{}
	h := NewHandler(s, nil, nil, nil, cfg)
	agents := &mockProviderAgentController{}
	h.agentManager = agents

	create := httptest.NewRequest(http.MethodPost, "/api/opencode/accounts", strings.NewReader(`{"name":"Work","workspace_id":"ws-1","auth_cookie":"secret-cookie","enabled":true}`))
	rr := httptest.NewRecorder()
	h.OpenCodeAccounts(rr, create)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "secret-cookie") || strings.Contains(rr.Body.String(), "ciphertext") {
		t.Fatalf("create leaked credential: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.OpenCodeAccounts(rr, httptest.NewRequest(http.MethodGet, "/api/opencode/accounts", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"has_auth_cookie":true`) {
		t.Fatalf("list response=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "secret-cookie") {
		t.Fatalf("list leaked credential: %s", rr.Body.String())
	}
	if len(agents.started) != 1 || agents.started[0] != "opencode" {
		t.Fatalf("creating the first account did not start OpenCode polling: %+v", agents.started)
	}
	accounts, err := s.QueryOpenCodeAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("created accounts=%+v err=%v", accounts, err)
	}
	accountID := accounts[0].AccountID

	rr = httptest.NewRecorder()
	updateURL := "/api/opencode/accounts?id=" + strconv.FormatInt(accountID, 10)
	update := httptest.NewRequest(http.MethodPut, updateURL, strings.NewReader(`{"name":"Work renamed","workspace_id":"ws-1","auth_cookie":"replacement-cookie","enabled":false}`))
	h.OpenCodeAccounts(rr, update)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "replacement-cookie") || !strings.Contains(rr.Body.String(), `"auth_status":"disabled"`) {
		t.Fatalf("update response=%d %s", rr.Code, rr.Body.String())
	}
	secret, err := s.GetOpenCodeAccount(accountID, true)
	if err != nil || secret.AuthCookie != "replacement-cookie" || secret.Enabled {
		t.Fatalf("updated account=%+v err=%v", secret, err)
	}

	rr = httptest.NewRecorder()
	h.OpenCodeAccounts(rr, httptest.NewRequest(http.MethodDelete, updateURL, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete response=%d %s", rr.Code, rr.Body.String())
	}
	if cfg.OpenCodeAccountsConfigured {
		t.Fatal("deleting the last account did not clear configured provider state")
	}
	if len(agents.stopped) != 1 || agents.stopped[0] != "opencode" {
		t.Fatalf("deleting the last account did not stop OpenCode polling: %+v", agents.stopped)
	}
	deleted, err := s.GetOpenCodeAccount(accountID, false)
	if err != nil || deleted == nil || deleted.HasAuthCookie || deleted.DeletedAt == nil {
		t.Fatalf("deleted account retained credentials or was not soft-deleted: account=%+v err=%v", deleted, err)
	}
}

func TestOpenCodeCurrentIsAccountScopedAndSummaryIsCredentialSafe(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateOpenCodeAccount("A", "ws-a", "cookie-a", true)
	b, _ := s.CreateOpenCodeAccount("B", "ws-b", "cookie-b", true)
	now := time.Now().UTC()
	for _, tc := range []struct {
		id   int64
		util float64
	}{{a.AccountID, 10}, {b.AccountID, 90}} {
		_, err := s.InsertOpenCodeSnapshotForAccount(tc.id, &api.OpenCodeSnapshot{CapturedAt: now, Quotas: []api.OpenCodeQuota{{Name: "weekly", Used: tc.util, Limit: 100, Utilization: tc.util, Format: api.OpenCodeQuotaFormatPercent}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(s, nil, nil, nil, nil)
	rr := httptest.NewRecorder()
	h.currentOpenCode(rr, httptest.NewRequest(http.MethodGet, "/api/current?provider=opencode&account="+strconv.FormatInt(b.AccountID, 10), nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"utilization":90`) || strings.Contains(rr.Body.String(), `"utilization":10`) {
		t.Fatalf("account-scoped current response=%d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.OpenCodeAccountsSummary(rr, httptest.NewRequest(http.MethodGet, "/api/opencode/accounts/summary", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"accountId"`) {
		t.Fatalf("summary=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "cookie-a") || strings.Contains(rr.Body.String(), "cookie-b") || strings.Contains(rr.Body.String(), "ciphertext") {
		t.Fatalf("summary leaked credential: %s", rr.Body.String())
	}
}

func TestAllProvidersHistoryIncludesOpenCodeOnlyForExplicitAccount(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account, err := s.CreateOpenCodeAccount("A", "ws-a", "cookie-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertOpenCodeSnapshotForAccount(account.AccountID, &api.OpenCodeSnapshot{
		CapturedAt: time.Now().UTC(),
		Quotas:     []api.OpenCodeQuota{{Name: "weekly", Utilization: 37, Format: api.OpenCodeQuotaFormatPercent}},
	}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, nil, nil, nil, &config.Config{OpenCodeAccountsConfigured: true})

	rr := httptest.NewRecorder()
	h.historyBoth(rr, httptest.NewRequest(http.MethodGet, "/api/history?range=6h", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("all-provider history status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"opencode"`) {
		t.Fatalf("all-provider history loaded an implicit OpenCode account: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	url := "/api/history?range=6h&account=" + strconv.FormatInt(account.AccountID, 10)
	h.historyBoth(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"opencode"`) || !strings.Contains(rr.Body.String(), `"weekly":37`) {
		t.Fatalf("explicit-account history status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAllProvidersInsightsDoNotChooseImplicitOpenCodeAccount(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
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
	for _, accountID := range []int64{a.AccountID, b.AccountID} {
		if _, err := s.InsertOpenCodeSnapshotForAccount(accountID, &api.OpenCodeSnapshot{
			CapturedAt: time.Now().UTC(), PlanName: "OpenCode Go",
			Quotas: []api.OpenCodeQuota{{Name: "weekly", Utilization: 25, Format: api.OpenCodeQuotaFormatPercent}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(s, nil, nil, nil, &config.Config{OpenCodeAccountsConfigured: true})
	rr := httptest.NewRecorder()
	h.insightsBoth(rr, httptest.NewRequest(http.MethodGet, "/api/insights?provider=both", nil), 24*time.Hour)
	if rr.Code != http.StatusOK {
		t.Fatalf("all-provider insights status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"opencode"`) {
		t.Fatalf("all-provider insights chose an implicit OpenCode account: %s", rr.Body.String())
	}
}

func TestOpenCodeAccountAPIRequiresDashboardAuthentication(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	passwordHash, err := HashPassword("test-password")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(0, h, nil, "admin", passwordHash, "", "", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/api/opencode/accounts", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	rr := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated account API status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestGenericSettingsRejectsPlaintextOpenCodeCredential(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	body := `{"provider_settings":{"opencode":{"workspace_id":"ws-plaintext","auth_cookie":"plaintext-cookie"}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.UpdateSettings(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("plaintext OpenCode settings status=%d body=%s", rr.Code, rr.Body.String())
	}
	raw, err := s.GetSetting("provider_settings")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "plaintext-cookie") {
		t.Fatalf("generic settings persisted a plaintext OpenCode Cookie: %s", raw)
	}
}
