package web

import (
	"strings"
	"testing"
)

func TestAppJS_OpenCodeQuotaCardsRerenderWhenQuotaSetChanges(t *testing.T) {
	t.Parallel()

	appJS := readStaticAppJS(t)
	if !strings.Contains(appJS, "function openCodeQuotaSetsMatch(container, quotas)") {
		t.Fatal("OpenCode cards must compare rendered and incoming quota-name sets")
	}

	updateIdx := strings.Index(appJS, "} else if (provider === 'opencode') {")
	if updateIdx < 0 {
		t.Fatal("OpenCode current-usage update branch not found")
	}
	updateBody := appJS[updateIdx:]
	setMatchIdx := strings.Index(updateBody, "!openCodeQuotaSetsMatch(container, data.quotas)")
	updateCardsIdx := strings.Index(updateBody, "data.quotas.forEach(q => updateOpenCodeCard(q));")
	if setMatchIdx < 0 {
		t.Fatal("OpenCode cards must re-render when quota-name sets differ")
	}
	if updateCardsIdx < 0 || setMatchIdx > updateCardsIdx {
		t.Fatal("OpenCode quota set comparison must happen before in-place card updates")
	}
}

func TestAppJS_OpenCodeAllAccountsUsesSummaryAndBoundedAggregateHistory(t *testing.T) {
	t.Parallel()
	appJS := readStaticAppJS(t)
	if !strings.Contains(appJS, "provider === 'opencode' && State.openCodeAccount === 'all'") {
		t.Fatal("OpenCode all-account mode is not wired")
	}
	if !strings.Contains(appJS, "/api/opencode/accounts/summary") {
		t.Fatal("OpenCode all-account mode must use the latest-only summary endpoint")
	}
	if !strings.Contains(appJS, "/api/opencode/accounts/history?range=") || !strings.Contains(appJS, "renderOpenCodeAggregateChart") {
		t.Fatal("OpenCode all-account mode must load the bounded aggregate history endpoint")
	}
	if !strings.Contains(appJS, "account.authStatus") || !strings.Contains(appJS, "data-auth-status") {
		t.Fatal("OpenCode all-account cards must display each account authentication status")
	}
}

func TestAppJS_OpenCodeQuotasUseCanonicalDisplayOrder(t *testing.T) {
	t.Parallel()

	appJS := readStaticAppJS(t)
	if !strings.Contains(appJS, "function sortOpenCodeQuotas(quotas)") {
		t.Fatal("OpenCode quota rendering must use a shared canonical ordering helper")
	}
	if !strings.Contains(appJS, "sortOpenCodeQuotas(quotas).map((q, i) =>") {
		t.Fatal("OpenCode detail cards must render quotas in canonical order")
	}
	if !strings.Contains(appJS, "sortOpenCodeQuotas(account.quotas || []).map(q =>") {
		t.Fatal("OpenCode all-account summaries must render quotas in canonical order")
	}
}
