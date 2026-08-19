package web

import (
	"strings"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/config"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func TestBuildZaiCurrentTreatsEmptySnapshotAsPending(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	if _, err := s.InsertZaiSnapshot(&api.ZaiSnapshot{CapturedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("insert empty snapshot: %v", err)
	}

	h := NewHandler(s, nil, nil, nil, &config.Config{ZaiAPIKey: "test-key"})
	response := h.buildZaiCurrent()
	if hasData, _ := response["hasData"].(bool); hasData {
		t.Fatal("an all-zero Z.ai snapshot must not be presented as valid quota data")
	}
	for _, key := range []string{"tokensLimit", "timeLimit", "toolCalls"} {
		quota, ok := response[key].(map[string]interface{})
		if !ok {
			t.Fatalf("%s response is missing", key)
		}
		if quota["status"] != "pending" {
			t.Errorf("%s status = %v, want pending", key, quota["status"])
		}
	}
}

func TestBuildDeepSeekCurrentExposesDataAvailability(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	h := NewHandler(s, nil, nil, nil, &config.Config{DeepSeekAPIKey: "test-key"})
	empty := h.buildDeepSeekCurrent()
	if hasData, _ := empty["hasData"].(bool); hasData {
		t.Fatal("DeepSeek must report hasData=false before the first valid snapshot")
	}
	emptyBalance := empty["balance"].(map[string]interface{})
	if emptyBalance["status"] != "pending" {
		t.Fatalf("empty DeepSeek status = %v, want pending", emptyBalance["status"])
	}

	snapshot := &api.DeepSeekSnapshot{
		CapturedAt:      time.Now().UTC(),
		IsAvailable:     true,
		Currency:        "CNY",
		TotalBalance:    12.34,
		GrantedBalance:  2.34,
		ToppedUpBalance: 10,
	}
	if _, err := s.InsertDeepSeekSnapshot(snapshot); err != nil {
		t.Fatalf("insert DeepSeek snapshot: %v", err)
	}

	current := h.buildDeepSeekCurrent()
	if hasData, _ := current["hasData"].(bool); !hasData {
		t.Fatal("DeepSeek must report hasData=true after a valid snapshot")
	}
	balance := current["balance"].(map[string]interface{})
	if balance["total"] != 12.34 || balance["granted"] != 2.34 || balance["toppedUp"] != 10.0 {
		t.Fatalf("unexpected DeepSeek balance payload: %#v", balance)
	}
}

func TestProviderDashboardRecognizesAndRendersBalanceProviders(t *testing.T) {
	appJS := readStaticAppJS(t)
	for _, marker := range []string{
		"document.getElementById('quota-grid-deepseek')",
		"renderDeepSeekBalanceCards(data)",
		"renderProviderDataState('quota-grid', data.hasData, 'Z.ai')",
		"tr('dashboard.no_valid_provider_data'",
		"tr('dashboard.provider_data_help'",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("provider dashboard data handling missing %q", marker)
		}
	}

	i18n := readEmbeddedFile(t, "static/i18n.js")
	for _, key := range []string{"dashboard.no_valid_provider_data", "dashboard.provider_data_help", "deepseek.total_balance", "deepseek.granted_balance", "deepseek.topped_up_balance"} {
		if strings.Count(i18n, "'"+key+"':") != 2 {
			t.Errorf("translation key %q must exist in English and Simplified Chinese", key)
		}
	}
}

func TestZaiPendingStateHidesDataDependentDashboardSections(t *testing.T) {
	t.Parallel()
	appJS := readStaticAppJS(t)
	for _, marker := range []string{
		"setProviderDataSectionsVisible('zai', data.hasData)",
		"setProviderDataSectionsVisible('deepseek', hasData)",
		"data-requires-provider-data=\"${provider}\"",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("Z.ai pending-state handling missing %q", marker)
		}
	}

	dashboard := readEmbeddedFile(t, "templates/dashboard.html")
	if strings.Count(dashboard, `data-requires-provider-data="zai"`) < 3 {
		t.Fatal("Z.ai insights, trend, and history sections must be marked as data-dependent")
	}
	if strings.Count(dashboard, `data-requires-provider-data="deepseek"`) < 3 {
		t.Fatal("DeepSeek insights, trend, and history sections must be marked as data-dependent")
	}
}

func TestBuildZaiCurrentLabelsCodingPlanCreditWindows(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.InsertZaiSnapshot(&api.ZaiSnapshot{CapturedAt: time.Now().UTC(), TimeUnit: 3, TimeNumber: 5, TimeUsage: 1000, TimeCurrentValue: 250, TimePercentage: 25, TokensUnit: 6, TokensNumber: 1, TokensUsage: 5000, TokensCurrentValue: 2000, TokensPercentage: 40}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, nil, nil, nil, &config.Config{ZaiAPIKey: "test"})
	current := h.buildZaiCurrent()
	if current["codingPlan"] != true {
		t.Fatalf("expected Coding Plan marker: %#v", current)
	}
	if current["timeLimit"].(map[string]interface{})["name"] != "5-Hour Coding Plan" {
		t.Fatalf("unexpected short-window label: %#v", current["timeLimit"])
	}
	if current["tokensLimit"].(map[string]interface{})["name"] != "Weekly Coding Plan" {
		t.Fatalf("unexpected weekly label: %#v", current["tokensLimit"])
	}
}

func TestOpenCodeAllAccountsUsesOpenCodeHistoryAndKeepsBoundedCombinedTables(t *testing.T) {
	appJS := readStaticAppJS(t)
	for _, expected := range []string{
		"provider === 'opencode' ? `${API_BASE}/api/opencode/accounts/summary`",
		"/api/opencode/accounts/history?range=",
		"renderOpenCodeAggregateChart",
		"provider === 'opencode' ? 500",
		"provider === 'opencode');",
		"openCodeAggregateCardHTML",
		".account-overview-card[data-account-id]",
		"tr('table.max')",
	} {
		if !strings.Contains(appJS, expected) {
			t.Fatalf("app.js missing OpenCode all-accounts behavior %q", expected)
		}
	}
	if strings.Contains(appJS, "if (requestProvider === 'opencode') {\n      if (State.chart)") {
		t.Fatal("OpenCode all-accounts history must not be discarded before rendering")
	}
}

func TestOpenCodeDashboardShowsPaceMarkersAndLocalizesDynamicInsights(t *testing.T) {
	appJS := readStaticAppJS(t)
	for _, marker := range []string{
		"quotaThresholdMarkersHTML",
		"paceWarningMarker",
		"paceCriticalMarker",
		"insights.burn_rate_title",
		"insights.stable_cycle_desc",
		"localizedInsightText(displayMetric)",
		"localizedQuotaLabel(q.name, q.displayName || opencodeDisplayNames[q.name] || q.name)",
	} {
		if !strings.Contains(appJS, marker) {
			t.Fatalf("app.js missing OpenCode dashboard behavior %q", marker)
		}
	}
	i18n := readEmbeddedFile(t, "static/i18n.js")
	for _, key := range []string{"insights.burn_rate_title", "insights.stable_cycle_desc", "insights.per_hour_metric", "quota.pace_warning_marker", "api_integrations.provider_accounts_separate"} {
		if strings.Count(i18n, "'"+key+"':") != 2 {
			t.Fatalf("translation key %q must exist in English and Simplified Chinese", key)
		}
	}
	css := readEmbeddedFile(t, "static/style.css")
	for _, marker := range []string{".quota-threshold-marker", ".api-integrations-empty-state", ".api-integrations-insights-panels[hidden]", "justify-content: center"} {
		if !strings.Contains(css, marker) {
			t.Fatalf("style.css missing %q", marker)
		}
	}
}
