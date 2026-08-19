package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/config"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

type countingMiniMaxReloader struct{ count int }

func (r *countingMiniMaxReloader) Reload() { r.count++ }

func TestUpdateSettingsHotReloadsDeepSeekAndZai(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := &config.Config{}
	agents := &mockProviderAgentController{}
	h := NewHandler(s, nil, nil, nil, cfg)
	h.SetAgentManager(agents)

	body := `{"provider_settings":{"deepseek":{"api_key":"deep-secret"},"zai":{"api_key":"zai-secret","region":"cn"}}}`
	rr := httptest.NewRecorder()
	h.UpdateSettings(rr, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !h.isProviderConfigured("deepseek") || !h.isProviderConfigured("zai") {
		t.Fatalf("stored providers were not recognized as configured")
	}
	for _, provider := range []string{"deepseek", "zai"} {
		if !containsString(agents.stopped, provider) || !containsString(agents.started, provider) {
			t.Fatalf("%s was not restarted: stopped=%v started=%v", provider, agents.stopped, agents.started)
		}
	}
	if strings.Contains(rr.Body.String(), "deep-secret") || strings.Contains(rr.Body.String(), "zai-secret") {
		t.Fatalf("settings response leaked credentials: %s", rr.Body.String())
	}
}

func TestMiniMaxFirstAccountStartsPollingAndAppearsAsProvider(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agents := &mockProviderAgentController{}
	reloader := &countingMiniMaxReloader{}
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.SetAgentManager(agents)
	h.SetMiniMaxAgentManager(reloader)

	rr := httptest.NewRecorder()
	h.MiniMaxAccounts(rr, httptest.NewRequest(http.MethodPost, "/api/minimax/accounts", strings.NewReader(`{"name":"work","api_key":"secret-key","region":"global"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !containsString(agents.started, "minimax") || reloader.count == 0 {
		t.Fatalf("first MiniMax account did not start polling: started=%v reloads=%d", agents.started, reloader.count)
	}

	rr = httptest.NewRecorder()
	h.Providers(rr, httptest.NewRequest(http.MethodGet, "/api/providers", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"minimax"`) {
		t.Fatalf("MiniMax missing from providers: status=%d body=%s", rr.Code, rr.Body.String())
	}

	accounts, err := s.QueryActiveProviderAccounts("minimax")
	if err != nil || len(accounts) < 1 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	for _, account := range accounts {
		if strings.Contains(account.Metadata, "secret-key") {
			t.Fatalf("MiniMax API key stored as plaintext: %s", account.Metadata)
		}
	}
}

func TestZaiEmptyCurrentIsPendingInsteadOfHealthy(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{ZaiAPIKey: "configured"})
	current := h.buildZaiCurrent()
	if current["hasData"] != false {
		t.Fatalf("empty Z.ai response must advertise hasData=false: %+v", current)
	}
	for _, key := range []string{"tokensLimit", "timeLimit", "toolCalls"} {
		quota := current[key].(map[string]interface{})
		if quota["status"] != "pending" {
			t.Fatalf("%s status=%v, want pending", key, quota["status"])
		}
	}
}

func TestAccountConnectionTestsDoNotPersistCredentials(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.openCodeConnectionTest = func(_ context.Context, workspaceID, cookie string) error {
		if workspaceID != "ws-test" || cookie != "cookie-secret" {
			t.Fatalf("unexpected OpenCode test values")
		}
		return nil
	}
	h.miniMaxConnectionTest = func(_ context.Context, apiKey, region string) error {
		if apiKey != "mini-secret" || region != "cn" {
			t.Fatalf("unexpected MiniMax test values")
		}
		return nil
	}

	for _, tc := range []struct {
		path string
		body string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"/api/opencode/accounts/test", `{"workspace_id":"ws-test","auth_cookie":"cookie-secret"}`, h.OpenCodeAccountTest},
		{"/api/minimax/accounts/test", `{"api_key":"mini-secret","region":"cn"}`, h.MiniMaxAccountTest},
	} {
		rr := httptest.NewRecorder()
		tc.call(rr, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"success":true`) {
			t.Fatalf("test %s status=%d body=%s", tc.path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "secret") {
			t.Fatalf("test response leaked credentials: %s", rr.Body.String())
		}
	}
	openCode, _ := s.QueryOpenCodeAccounts(false)
	miniMax, _ := s.QueryActiveProviderAccounts("minimax")
	configuredMiniMax := 0
	for _, account := range miniMax {
		if account.Metadata != "" {
			configuredMiniMax++
		}
	}
	if len(openCode) != 0 || configuredMiniMax != 0 {
		t.Fatalf("connection tests persisted accounts: opencode=%v minimax=%v", openCode, miniMax)
	}
}

func TestProviderCredentialTestValidatesWithoutPersistingSecrets(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.providerCredentialConnectionTest = func(_ context.Context, provider string, values map[string]string) error {
		if provider != "zai" {
			t.Fatalf("unexpected provider credential test: provider=%q", provider)
		}
		if values["api_key"] != "zai-secret" || values["region"] != "cn" {
			t.Fatal("provider credential values were not normalized as expected")
		}
		return nil
	}

	rr := httptest.NewRecorder()
	h.ProviderCredentialTest(rr, httptest.NewRequest(http.MethodPost, "/api/providers/test", strings.NewReader(`{"provider":"zai","settings":{"api_key":"zai-secret","region":"cn"}}`)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"success":true`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "zai-secret") {
		t.Fatalf("response leaked API key: %s", rr.Body.String())
	}
	stored, _ := s.GetSetting("provider_settings")
	if stored != "" {
		t.Fatalf("connection test persisted settings: %s", stored)
	}
}

func TestProviderCredentialTestNormalizesSupportedProvidersAndErrors(t *testing.T) {
	for _, tc := range []struct {
		provider string
		settings map[string]string
	}{
		{"synthetic", map[string]string{"api_key": "secret"}},
		{"zai", map[string]string{"api_key": "secret", "region": "CN"}},
		{"copilot", map[string]string{"token": "secret"}},
		{"openrouter", map[string]string{"api_key": "secret"}},
		{"moonshot", map[string]string{"api_key": "secret"}},
		{"deepseek", map[string]string{"api_key": "secret"}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			values, ok := normalizeProviderCredentialValues(tc.provider, tc.settings)
			if !ok || len(values) == 0 {
				t.Fatalf("provider %s was not accepted", tc.provider)
			}
		})
	}
	if _, ok := normalizeProviderCredentialValues("unknown", map[string]string{"api_key": "secret"}); ok {
		t.Fatal("unsupported provider must be rejected")
	}
	if got := providerCredentialError(api.ErrZaiUnauthorized); got != "authentication_failed" {
		t.Fatalf("unauthorized mapping=%q", got)
	}
	if got := providerCredentialError(api.ErrZaiInvalidResponse); got != "quota_parse_failed" {
		t.Fatalf("invalid response mapping=%q", got)
	}
}

func TestAccountCSVTemplatesAndImports(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})

	rr := httptest.NewRecorder()
	h.OpenCodeAccountsTemplate(rr, httptest.NewRequest(http.MethodGet, "/api/opencode/accounts/template", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "name,workspace_id,auth_cookie,enabled") {
		t.Fatalf("OpenCode template status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	csvBody := "name,workspace_id,auth_cookie,enabled\n账号一,ws-1,cookie-1,true\n账号二,ws-2,cookie-2,false\n"
	h.OpenCodeAccountsImport(rr, httptest.NewRequest(http.MethodPost, "/api/opencode/accounts/import", strings.NewReader(csvBody)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"imported":2`) {
		t.Fatalf("OpenCode import status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "cookie-1") || strings.Contains(rr.Body.String(), "cookie-2") {
		t.Fatalf("import response leaked credentials: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.MiniMaxAccountsTemplate(rr, httptest.NewRequest(http.MethodGet, "/api/minimax/accounts/template", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "name,api_key,region,enabled") {
		t.Fatalf("MiniMax template status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.MiniMaxAccountsImport(rr, httptest.NewRequest(http.MethodPost, "/api/minimax/accounts/import", strings.NewReader("name,api_key,region,enabled\nmini,mini-key,cn,true\n")))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"imported":1`) {
		t.Fatalf("MiniMax import status=%d body=%s", rr.Code, rr.Body.String())
	}
}
