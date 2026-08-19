package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/notify"
	"github.com/onllm-dev/onwatch/v2/internal/store"
	"github.com/onllm-dev/onwatch/v2/internal/tracker"
)

const openCodeAccountRequestLimit = 32 << 10

type openCodeAccountRequest struct {
	Name        string `json:"name"`
	WorkspaceID string `json:"workspace_id"`
	AuthCookie  string `json:"auth_cookie"`
	Enabled     bool   `json:"enabled"`
}

func decodeOpenCodeAccountRequest(w http.ResponseWriter, r *http.Request) (openCodeAccountRequest, error) {
	var req openCodeAccountRequest
	r.Body = http.MaxBytesReader(w, r.Body, openCodeAccountRequestLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	req.Name, req.WorkspaceID, req.AuthCookie = strings.TrimSpace(req.Name), strings.TrimSpace(req.WorkspaceID), strings.TrimSpace(req.AuthCookie)
	if req.Name == "" || len(req.Name) > 80 || req.WorkspaceID == "" || len(req.WorkspaceID) > 256 || len(req.AuthCookie) > 16384 {
		return req, fmt.Errorf("invalid account fields")
	}
	return req, nil
}

// OpenCodeAccounts manages encrypted OpenCode Go account credentials.
func (h *Handler) OpenCodeAccounts(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		accounts, err := h.store.QueryOpenCodeAccounts(false)
		if err != nil {
			h.logger.Error("failed to list OpenCode accounts", "error", err)
			respondError(w, http.StatusInternalServerError, "failed to list accounts")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
	case http.MethodPost:
		req, err := decodeOpenCodeAccountRequest(w, r)
		if err != nil || req.AuthCookie == "" {
			respondError(w, http.StatusBadRequest, "name, workspace_id, and auth_cookie are required")
			return
		}
		account, err := h.store.CreateOpenCodeAccount(req.Name, req.WorkspaceID, req.AuthCookie, req.Enabled)
		if err != nil {
			h.logger.Warn("failed to create OpenCode account", "error", err)
			if errors.Is(err, store.ErrOpenCodeWorkspaceExists) {
				respondJSON(w, http.StatusConflict, map[string]any{"error": "workspace_exists"})
				return
			}
			respondError(w, http.StatusConflict, "could not create account")
			return
		}
		if h.config != nil {
			h.config.OpenCodeAccountsConfigured = true
		}
		if h.agentManager != nil && h.providerPollingEnabled("opencode", h.providerVisibilityMap()) {
			if err := h.agentManager.Start("opencode"); err != nil {
				h.logger.Warn("failed to start OpenCode polling after account creation", "error", err)
			}
		}
		respondJSON(w, http.StatusCreated, account)
	case http.MethodPut:
		accountID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || accountID <= 0 {
			respondError(w, http.StatusBadRequest, "valid id is required")
			return
		}
		existing, err := h.store.GetOpenCodeAccount(accountID, false)
		if err != nil || existing == nil || existing.DeletedAt != nil {
			respondError(w, http.StatusNotFound, "account not found")
			return
		}
		req, err := decodeOpenCodeAccountRequest(w, r)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid account")
			return
		}
		var cookie *string
		if req.AuthCookie != "" {
			cookie = &req.AuthCookie
		}
		account, err := h.store.UpdateOpenCodeAccount(accountID, req.Name, req.WorkspaceID, cookie, req.Enabled)
		if err != nil {
			h.logger.Warn("failed to update OpenCode account", "account_id", accountID, "error", err)
			respondError(w, http.StatusConflict, "could not update account")
			return
		}
		respondJSON(w, http.StatusOK, account)
	case http.MethodDelete:
		accountID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || accountID <= 0 {
			respondError(w, http.StatusBadRequest, "valid id is required")
			return
		}
		existing, err := h.store.GetOpenCodeAccount(accountID, false)
		if err != nil || existing == nil || existing.DeletedAt != nil {
			respondError(w, http.StatusNotFound, "account not found")
			return
		}
		if err := h.store.DeleteOpenCodeAccount(accountID); err != nil {
			h.logger.Error("failed to delete OpenCode account", "account_id", accountID, "error", err)
			respondError(w, http.StatusInternalServerError, "could not delete account")
			return
		}
		if h.config != nil {
			accounts, queryErr := h.store.QueryOpenCodeAccounts(false)
			if queryErr == nil {
				h.config.OpenCodeAccountsConfigured = len(accounts) > 0
				if len(accounts) == 0 && h.agentManager != nil {
					h.agentManager.Stop("opencode")
				}
			}
		}
		respondJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// OpenCodeAccountsSummary returns one latest snapshot per account and never historical series.
func (h *Handler) OpenCodeAccountsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondJSON(w, http.StatusOK, map[string]any{"accounts": []any{}})
		return
	}
	summaries, err := h.store.QueryOpenCodeAccountSummaries()
	if err != nil {
		h.logger.Error("failed to query OpenCode account summaries", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query summaries")
		return
	}
	now := time.Now()
	paceCfg := h.openCodePaceConfig()
	accounts := make([]map[string]any, 0, len(summaries))
	for _, summary := range summaries {
		entry := map[string]any{
			"id": summary.Account.AccountID, "accountId": summary.Account.AccountID,
			"name": summary.Account.Name, "accountName": summary.Account.Name,
			"workspaceId": summary.Account.WorkspaceID, "enabled": summary.Account.Enabled,
			"authStatus": summary.Account.AuthStatus, "quotas": []map[string]any{},
		}
		if summary.Snapshot != nil {
			entry["capturedAt"] = summary.Snapshot.CapturedAt.Format(time.RFC3339)
			entry["planName"] = summary.Snapshot.PlanName
			quotas := make([]map[string]any, 0, len(summary.Snapshot.Quotas))
			for _, q := range summary.Snapshot.Quotas {
				item := map[string]any{"name": q.Name, "displayName": opencodeDisplayName(q.Name), "utilization": q.Utilization, "used": q.Used, "limit": q.Limit, "format": q.Format, "status": utilStatus(q.Utilization)}
				if q.ResetsAt != nil {
					item["resetsAt"] = q.ResetsAt.Format(time.RFC3339)
					applyOpenCodePaceMarkers(item, q.Name, *q.ResetsAt, paceCfg, now)
				}
				quotas = append(quotas, item)
			}
			entry["quotas"] = quotas
		}
		accounts = append(accounts, entry)
	}
	respondJSON(w, http.StatusOK, map[string]any{"accounts": accounts, "aggregate": h.buildOpenCodeSummaryAggregate(summaries, paceCfg, now)})
}

func (h *Handler) buildOpenCodeSummaryAggregate(summaries []store.OpenCodeAccountSummary, paceCfg notify.PaceConfig, now time.Time) map[string]any {
	type quotaAccumulator struct {
		total         float64
		count         int
		warningTotal  float64
		criticalTotal float64
		markerCount   int
	}
	acc := make(map[string]quotaAccumulator)
	sampledAccounts := 0
	for _, summary := range summaries {
		if summary.Snapshot == nil {
			continue
		}
		sampledAccounts++
		seen := make(map[string]bool)
		for _, quota := range summary.Snapshot.Quotas {
			if seen[quota.Name] {
				continue
			}
			seen[quota.Name] = true
			value := acc[quota.Name]
			value.total += quota.Utilization
			value.count++
			if quota.ResetsAt != nil {
				warning, critical, _, ok := notify.PaceMarkers(quota.Name, *quota.ResetsAt, paceCfg, now)
				if ok {
					value.warningTotal += warning
					value.criticalTotal += critical
					value.markerCount++
				}
			}
			acc[quota.Name] = value
		}
	}
	names := make([]string, 0, len(acc))
	for name := range acc {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return opencodeQuotaOrder(names[i]) < opencodeQuotaOrder(names[j]) })
	quotas := make([]map[string]any, 0, len(names))
	for _, name := range names {
		value := acc[name]
		if value.count == 0 {
			continue
		}
		average := value.total / float64(value.count)
		item := map[string]any{
			"name": name, "displayName": opencodeDisplayName(name),
			"averageUtilization": average, "utilization": average, "sampleCount": value.count,
			"status": utilStatus(average),
		}
		if value.markerCount > 0 {
			item["paceWarningMarker"] = value.warningTotal / float64(value.markerCount)
			item["paceCriticalMarker"] = value.criticalTotal / float64(value.markerCount)
			item["paceMarkerSampleCount"] = value.markerCount
		}
		quotas = append(quotas, item)
	}
	return map[string]any{"accountCount": len(summaries), "sampledAccountCount": sampledAccounts, "quotas": quotas}
}

func defaultOpenCodePaceConfig() notify.PaceConfig {
	return notify.PaceConfig{Enabled: true, Warning: 10, Critical: 20, WorkdayStart: "09:00", LunchStart: "12:00", LunchMinutes: 60, WorkdayEnd: "18:00", WorkdaysPerWeek: 5}
}

func (h *Handler) openCodePaceConfig() notify.PaceConfig {
	cfg := defaultOpenCodePaceConfig()
	if h.store == nil {
		return cfg
	}
	if raw, err := h.store.GetSetting("notifications"); err == nil && raw != "" {
		var saved struct {
			Pace *notify.PaceConfig `json:"pace"`
		}
		if json.Unmarshal([]byte(raw), &saved) == nil && saved.Pace != nil {
			cfg = *saved.Pace
		}
	}
	if timezone, err := h.store.GetSetting("timezone"); err == nil {
		cfg.Timezone = strings.TrimSpace(timezone)
	}
	return cfg
}

func applyOpenCodePaceMarkers(target map[string]any, quotaName string, resetsAt time.Time, cfg notify.PaceConfig, now time.Time) {
	warning, critical, progress, ok := notify.PaceMarkers(quotaName, resetsAt, cfg, now)
	if !ok {
		return
	}
	target["paceWarningMarker"] = warning
	target["paceCriticalMarker"] = critical
	target["paceWorkProgress"] = progress
}

// SetOpenCodeTracker sets the OpenCode tracker for usage summary enrichment.
func (h *Handler) SetOpenCodeTracker(t *tracker.OpenCodeTracker) {
	h.opencodeTracker = t
}

func (h *Handler) openCodeAccountID(r *http.Request) (int64, error) {
	if h.store == nil {
		return 0, fmt.Errorf("storage unavailable")
	}
	raw := strings.TrimSpace(r.URL.Query().Get("account"))
	if raw == "" {
		return h.store.DefaultOpenCodeAccountID()
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid account")
	}
	account, err := h.store.GetOpenCodeAccount(id, false)
	if err != nil || account == nil || account.DeletedAt != nil {
		return 0, fmt.Errorf("account not found")
	}
	return id, nil
}

func (h *Handler) currentOpenCode(w http.ResponseWriter, r *http.Request) {
	accountID, err := h.openCodeAccountID(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, h.buildOpenCodeCurrentForAccount(accountID))
}

// opencodeInsightsResponse is the JSON payload for OpenCode deep insights.
type opencodeInsightsResponse struct {
	Stats    []opencodeInsightStat `json:"stats"`
	Insights []insightItem         `json:"insights"`
}

// opencodeInsightStat is a stats-row shape that carries linked forecast metadata for the OpenCode dashboard.
type opencodeInsightStat struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Sublabel string `json:"sublabel,omitempty"`
	Key      string `json:"key,omitempty"`
	Metric   string `json:"metric,omitempty"`
	Severity string `json:"severity,omitempty"`
	Desc     string `json:"desc,omitempty"`
}

var opencodeQuotaDisplayOrder = map[string]int{
	"five_hour": 1,
	"weekly":    2,
	"monthly":   3,
}

var opencodeDisplayNames = map[string]string{
	"five_hour": "5-Hour",
	"weekly":    "Weekly",
	"monthly":   "Monthly",
}

func opencodeDisplayName(name string) string {
	if dn, ok := opencodeDisplayNames[name]; ok {
		return dn
	}
	return name
}

func opencodeQuotaOrder(name string) int {
	if order, ok := opencodeQuotaDisplayOrder[name]; ok {
		return order
	}
	return 99
}

type opencodeQuotaRate struct {
	Rate          float64
	HasRate       bool
	TimeToReset   time.Duration
	TimeToExhaust time.Duration
	ExhaustsFirst bool
	ProjectedPct  float64
}

func (h *Handler) computeOpenCodeRate(quotaName string, currentUtil float64, summary *tracker.OpenCodeSummary) opencodeQuotaRate {
	accountID, err := h.store.DefaultOpenCodeAccountID()
	if err != nil {
		return opencodeQuotaRate{}
	}
	return h.computeOpenCodeRateForAccount(accountID, quotaName, currentUtil, summary)
}

func (h *Handler) computeOpenCodeRateForAccount(accountID int64, quotaName string, currentUtil float64, summary *tracker.OpenCodeSummary) opencodeQuotaRate {
	var result opencodeQuotaRate

	if summary != nil && summary.ResetsAt != nil {
		result.TimeToReset = time.Until(*summary.ResetsAt)
	}

	if h.store != nil {
		points, err := h.store.QueryOpenCodeUtilizationSeriesForAccount(accountID, quotaName, time.Now().Add(-30*time.Minute), 200)
		if err == nil && len(points) >= 2 {
			first := points[0]
			last := points[len(points)-1]
			elapsed := last.CapturedAt.Sub(first.CapturedAt)
			if elapsed >= 5*time.Minute {
				delta := last.Utilization - first.Utilization
				if delta > 0 {
					result.Rate = delta / elapsed.Hours()
					result.HasRate = true
				} else {
					result.HasRate = true
				}
			}
		}
	}

	if !result.HasRate && summary != nil && summary.CurrentRate > 0 {
		result.Rate = summary.CurrentRate
		result.HasRate = true
	}

	if result.HasRate && result.Rate > 0 {
		remaining := 100 - currentUtil
		if remaining > 0 {
			result.TimeToExhaust = time.Duration(remaining / result.Rate * float64(time.Hour))
		}
		if result.TimeToReset > 0 {
			result.ProjectedPct = currentUtil + (result.Rate * result.TimeToReset.Hours())
			if result.ProjectedPct > 100 {
				result.ProjectedPct = 100
			}
			result.ExhaustsFirst = result.TimeToExhaust > 0 && result.TimeToExhaust < result.TimeToReset
		}
	}

	return result
}

func buildOpenCodeBurnRateInsight(quota api.OpenCodeQuota, rate opencodeQuotaRate) insightItem {
	item := insightItem{
		Key:   fmt.Sprintf("forecast_%s", quota.Name),
		Title: fmt.Sprintf("%s Burn Rate", opencodeDisplayName(quota.Name)),
	}

	resetStr := ""
	if rate.TimeToReset > 0 {
		resetStr = formatDuration(rate.TimeToReset)
	}
	projected := quota.Utilization
	if rate.ProjectedPct > projected {
		projected = rate.ProjectedPct
	}
	sublabel := fmt.Sprintf("~%.0f%% by reset", projected)
	if resetStr != "" {
		sublabel = fmt.Sprintf("~%.0f%% by reset in %s", projected, resetStr)
	}

	if !rate.HasRate {
		item.Type = "forecast"
		item.Severity = "info"
		item.Metric = "Analyzing..."
		item.Sublabel = sublabel
		item.Desc = fmt.Sprintf("Currently at %.0f%%. Collecting more snapshots to estimate burn rate and refine reset projection.", quota.Utilization)
		return item
	}

	if rate.Rate < 0.01 {
		item.Type = "forecast"
		item.Severity = "positive"
		item.Metric = "Idle"
		item.Sublabel = sublabel
		item.Desc = fmt.Sprintf("Currently at %.0f%%. No meaningful burn detected recently, so this quota looks stable through the rest of the cycle.", quota.Utilization)
		return item
	}

	item.Type = "forecast"
	item.Metric = fmt.Sprintf("%.1f%%/hr", rate.Rate)
	if rate.ExhaustsFirst {
		exhaustStr := formatDuration(rate.TimeToExhaust)
		item.Severity = "negative"
		item.Sublabel = sublabel
		item.Desc = fmt.Sprintf("Currently at %.0f%%. At this rate, projected %.0f%% by reset and likely to exhaust in %s before reset.", quota.Utilization, projected, exhaustStr)
		return item
	}

	if rate.ProjectedPct >= 80 {
		item.Severity = "warning"
		item.Sublabel = sublabel
		item.Desc = fmt.Sprintf("Currently at %.0f%%. At this rate, projected %.0f%% by reset.", quota.Utilization, projected)
		return item
	}

	item.Severity = "positive"
	item.Sublabel = sublabel
	item.Desc = fmt.Sprintf("Currently at %.0f%%. At this rate, projected %.0f%% by reset.", quota.Utilization, projected)
	return item
}

func (h *Handler) buildOpenCodeCurrent() map[string]interface{} {
	if h.store == nil {
		return map[string]interface{}{"capturedAt": time.Now().UTC().Format(time.RFC3339), "quotas": []interface{}{}}
	}
	accountID, err := h.store.DefaultOpenCodeAccountID()
	if err != nil {
		return map[string]interface{}{"capturedAt": time.Now().UTC().Format(time.RFC3339), "quotas": []interface{}{}}
	}
	return h.buildOpenCodeCurrentForAccount(accountID)
}

func (h *Handler) buildOpenCodeAggregateCurrent() map[string]interface{} {
	now := time.Now().UTC()
	response := map[string]interface{}{"capturedAt": now.Format(time.RFC3339), "quotas": []interface{}{}, "accountCount": 0}
	if h.store == nil {
		return response
	}
	summaries, err := h.store.QueryOpenCodeAccountSummaries()
	if err != nil {
		return response
	}
	response["accountCount"] = len(summaries)
	var latest time.Time
	needsReauth := 0
	for _, summary := range summaries {
		if summary.Account.AuthStatus == "needs_reauth" || summary.Account.AuthStatus == "unauthorized" {
			needsReauth++
		}
		if summary.Snapshot == nil {
			continue
		}
		if summary.Snapshot.CapturedAt.After(latest) {
			latest = summary.Snapshot.CapturedAt
		}
	}
	response["needsReauthCount"] = needsReauth
	if !latest.IsZero() {
		response["capturedAt"] = latest.Format(time.RFC3339)
	}
	aggregate := h.buildOpenCodeSummaryAggregate(summaries, h.openCodePaceConfig(), now)
	response["sampledAccountCount"] = aggregate["sampledAccountCount"]
	response["quotas"] = aggregate["quotas"]
	return response
}

func (h *Handler) buildOpenCodeCurrentForAccount(accountID int64) map[string]interface{} {
	now := time.Now().UTC()
	paceCfg := h.openCodePaceConfig()
	response := map[string]interface{}{
		"capturedAt": now.Format(time.RFC3339),
		"quotas":     []interface{}{},
	}

	if h.store == nil {
		return response
	}

	latest, err := h.store.QueryLatestOpenCodeForAccount(accountID)
	if err != nil || latest == nil {
		return response
	}

	response["capturedAt"] = latest.CapturedAt.Format(time.RFC3339)
	response["accountType"] = string(latest.AccountType)
	response["planName"] = latest.PlanName

	latestPerQuota, err := h.store.QueryOpenCodeLatestPerQuotaForAccount(accountID)
	if err != nil || len(latestPerQuota) == 0 {
		for _, q := range latest.Quotas {
			quotaMap := map[string]interface{}{
				"name":          q.Name,
				"displayName":   opencodeDisplayName(q.Name),
				"utilization":   q.Utilization,
				"used":          q.Used,
				"limit":         q.Limit,
				"format":        string(q.Format),
				"status":        utilStatus(q.Utilization),
				"lastUpdatedAt": latest.CapturedAt.Format(time.RFC3339),
				"ageSeconds":    int64(now.Sub(latest.CapturedAt).Seconds()),
			}
			if q.ResetsAt != nil {
				timeUntilReset := time.Until(*q.ResetsAt)
				quotaMap["resetsAt"] = q.ResetsAt.Format(time.RFC3339)
				quotaMap["timeUntilReset"] = formatDuration(timeUntilReset)
				quotaMap["timeUntilResetSeconds"] = int64(timeUntilReset.Seconds())
				applyOpenCodePaceMarkers(quotaMap, q.Name, *q.ResetsAt, paceCfg, now)
			}
			if h.opencodeTracker != nil {
				if summary, sErr := h.opencodeTracker.UsageSummaryForAccount(accountID, q.Name); sErr == nil && summary != nil {
					quotaMap["currentRate"] = summary.CurrentRate
					quotaMap["projectedUtil"] = summary.ProjectedUtil
				}
			}
			response["quotas"] = append(response["quotas"].([]interface{}), quotaMap)
		}
		applyDisplayModeToResponse(response, h.getDisplayMode("opencode"))
		return response
	}

	sort.SliceStable(latestPerQuota, func(i, j int) bool {
		left := opencodeQuotaOrder(latestPerQuota[i].Name)
		right := opencodeQuotaOrder(latestPerQuota[j].Name)
		if left != right {
			return left < right
		}
		return latestPerQuota[i].Name < latestPerQuota[j].Name
	})

	var quotas []interface{}
	for _, q := range latestPerQuota {
		age := now.Sub(q.CapturedAt)
		qMap := map[string]interface{}{
			"name":          q.Name,
			"displayName":   opencodeDisplayName(q.Name),
			"utilization":   q.Utilization,
			"used":          q.Used,
			"limit":         q.Limit,
			"format":        q.Format,
			"status":        utilStatus(q.Utilization),
			"lastUpdatedAt": q.CapturedAt.Format(time.RFC3339),
			"ageSeconds":    int64(age.Seconds()),
			"isStale":       age > 30*time.Minute,
		}
		if q.ResetsAt != nil {
			timeUntilReset := time.Until(*q.ResetsAt)
			qMap["resetsAt"] = q.ResetsAt.Format(time.RFC3339)
			qMap["timeUntilReset"] = formatDuration(timeUntilReset)
			qMap["timeUntilResetSeconds"] = int64(timeUntilReset.Seconds())
			applyOpenCodePaceMarkers(qMap, q.Name, *q.ResetsAt, paceCfg, now)
		}
		if h.opencodeTracker != nil {
			if summary, sErr := h.opencodeTracker.UsageSummaryForAccount(accountID, q.Name); sErr == nil && summary != nil {
				qMap["currentRate"] = summary.CurrentRate
				qMap["projectedUtil"] = summary.ProjectedUtil
			}
		}
		quotas = append(quotas, qMap)
	}
	response["quotas"] = quotas
	applyDisplayModeToResponse(response, h.getDisplayMode("opencode"))
	return response
}

func openCodeHistoryWindow(rangeParam string, now time.Time) (time.Time, time.Duration) {
	now = now.UTC()
	switch rangeParam {
	case "1h":
		return now.Add(-time.Hour), 5 * time.Minute
	case "6h":
		return now.Add(-6 * time.Hour), 5 * time.Minute
	case "24h", "1d":
		return now.Add(-24 * time.Hour), 15 * time.Minute
	case "3d":
		return now.Add(-3 * 24 * time.Hour), 30 * time.Minute
	case "30d":
		return now.Add(-30 * 24 * time.Hour), 4 * time.Hour
	default:
		return now.Add(-7 * 24 * time.Hour), time.Hour
	}
}

// OpenCodeAccountsHistory returns a bounded, downsampled average trend for the
// all-accounts dashboard. It never returns per-account history or credentials.
func (h *Handler) OpenCodeAccountsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondJSON(w, http.StatusOK, []any{})
		return
	}
	now := time.Now().UTC()
	start, bucketSize := openCodeHistoryWindow(r.URL.Query().Get("range"), now)
	points, err := h.store.QueryOpenCodeAggregateHistory(start, now, bucketSize, 200)
	if err != nil {
		h.logger.Error("failed to query aggregate OpenCode history", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query aggregate history")
		return
	}
	type historyEntry struct {
		CapturedAt string           `json:"capturedAt"`
		Quotas     []map[string]any `json:"quotas"`
	}
	entries := make([]historyEntry, 0)
	for _, point := range points {
		if len(entries) == 0 || entries[len(entries)-1].CapturedAt != point.CapturedAt.Format(time.RFC3339) {
			entries = append(entries, historyEntry{CapturedAt: point.CapturedAt.Format(time.RFC3339), Quotas: []map[string]any{}})
		}
		entries[len(entries)-1].Quotas = append(entries[len(entries)-1].Quotas, map[string]any{
			"name": point.QuotaName, "displayName": opencodeDisplayName(point.QuotaName),
			"utilization": point.AverageUtilization, "sampleCount": point.SampleCount,
		})
	}
	respondJSON(w, http.StatusOK, entries)
}

func (h *Handler) historyOpenCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.store == nil {
		respondJSON(w, http.StatusOK, []interface{}{})
		return
	}

	now := time.Now().UTC()
	start, _ := openCodeHistoryWindow(r.URL.Query().Get("range"), now)

	accountID, accountErr := h.openCodeAccountID(r)
	if accountErr != nil {
		respondError(w, http.StatusBadRequest, accountErr.Error())
		return
	}
	snapshots, err := h.store.QueryOpenCodeRangeForAccount(accountID, start, now, 200)
	if err != nil {
		h.logger.Error("failed to query OpenCode history", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query history")
		return
	}

	type historyEntry struct {
		CapturedAt string                   `json:"capturedAt"`
		Quotas     []map[string]interface{} `json:"quotas"`
	}

	result := make([]historyEntry, 0, len(snapshots))
	for _, snap := range snapshots {
		entry := historyEntry{
			CapturedAt: snap.CapturedAt.Format(time.RFC3339),
		}
		for _, q := range snap.Quotas {
			qMap := map[string]interface{}{
				"name":        q.Name,
				"utilization": q.Utilization,
				"used":        q.Used,
				"limit":       q.Limit,
				"format":      string(q.Format),
			}
			if q.ResetsAt != nil {
				qMap["resetsAt"] = q.ResetsAt.Format(time.RFC3339)
			}
			entry.Quotas = append(entry.Quotas, qMap)
		}
		result = append(result, entry)
	}

	respondJSON(w, http.StatusOK, result)
}

func (h *Handler) cyclesOpenCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.store == nil {
		respondJSON(w, http.StatusOK, []interface{}{})
		return
	}

	quotaName := r.URL.Query().Get("type")
	if quotaName == "" {
		quotaName = "five_hour"
	}

	accountID, accountErr := h.openCodeAccountID(r)
	if accountErr != nil {
		respondError(w, http.StatusBadRequest, accountErr.Error())
		return
	}
	active, err := h.store.QueryActiveOpenCodeCycleForAccount(accountID, quotaName)
	if err != nil {
		h.logger.Error("failed to query active OpenCode cycle", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query cycles")
		return
	}

	history, err := h.store.QueryOpenCodeCycleHistoryForAccount(accountID, quotaName, 50)
	if err != nil {
		h.logger.Error("failed to query OpenCode cycle history", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query cycles")
		return
	}

	var cycles []map[string]interface{}
	if active != nil {
		cycleMap := map[string]interface{}{
			"id":              active.ID,
			"quotaName":       active.QuotaName,
			"cycleStart":      active.CycleStart.Format(time.RFC3339),
			"cycleEnd":        nil,
			"peakUtilization": active.PeakUtilization,
			"totalDelta":      active.TotalDelta,
			"isActive":        true,
		}
		if active.ResetsAt != nil {
			cycleMap["resetsAt"] = active.ResetsAt.Format(time.RFC3339)
			cycleMap["timeUntilReset"] = formatDuration(time.Until(*active.ResetsAt))
		}
		cycles = append(cycles, cycleMap)
	}

	for _, c := range history {
		cycleMap := map[string]interface{}{
			"id":              c.ID,
			"quotaName":       c.QuotaName,
			"cycleStart":      c.CycleStart.Format(time.RFC3339),
			"cycleEnd":        c.CycleEnd.Format(time.RFC3339),
			"peakUtilization": c.PeakUtilization,
			"totalDelta":      c.TotalDelta,
			"isActive":        false,
		}
		if c.ResetsAt != nil {
			cycleMap["resetsAt"] = c.ResetsAt.Format(time.RFC3339)
		}
		cycles = append(cycles, cycleMap)
	}

	respondJSON(w, http.StatusOK, cycles)
}

func (h *Handler) cycleOverviewOpenCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.store == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"provider": "opencode", "groupBy": "five_hour",
			"quotaNames": []string{}, "cycles": []interface{}{},
		})
		return
	}

	groupBy := r.URL.Query().Get("groupBy")
	if groupBy == "" {
		groupBy = r.URL.Query().Get("group_by") // backward compatibility
	}
	if groupBy == "" {
		groupBy = "five_hour"
	}

	accountID, accountErr := h.openCodeAccountID(r)
	if accountErr != nil {
		respondError(w, http.StatusBadRequest, accountErr.Error())
		return
	}
	overview, err := h.store.QueryOpenCodeCycleOverviewForAccount(accountID, groupBy, parseCycleOverviewLimit(r))
	if err != nil {
		h.logger.Error("failed to query OpenCode cycle overview", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query cycle overview")
		return
	}
	quotaNames, err := h.store.QueryAllOpenCodeQuotaNamesForAccount(accountID)
	if err != nil {
		h.logger.Error("failed to query OpenCode quota names", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query quota names")
		return
	}
	sort.SliceStable(quotaNames, func(i, j int) bool {
		return opencodeQuotaOrder(quotaNames[i]) < opencodeQuotaOrder(quotaNames[j])
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"provider": "opencode", "groupBy": groupBy,
		"quotaNames": quotaNames, "cycles": cycleOverviewRowsToJSON(overview),
	})
}

func (h *Handler) summaryOpenCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	accountID, err := h.openCodeAccountID(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, h.buildOpenCodeSummaryMapForAccount(accountID))
}

func (h *Handler) insightsOpenCode(w http.ResponseWriter, r *http.Request, rangeDur time.Duration) {
	accountID, err := h.openCodeAccountID(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	hidden := h.getHiddenInsightKeys()
	respondJSON(w, http.StatusOK, h.buildOpenCodeInsightsForAccount(accountID, hidden, rangeDur))
}

func (h *Handler) opencodeQuotaNames() []string {
	if h.store == nil {
		return nil
	}
	names, err := h.store.QueryAllOpenCodeQuotaNames()
	if err != nil {
		return nil
	}
	return names
}

func (h *Handler) buildOpenCodeSummaryMap() map[string]interface{} {
	if h.store == nil {
		return map[string]interface{}{}
	}
	accountID, err := h.store.DefaultOpenCodeAccountID()
	if err != nil {
		return map[string]interface{}{}
	}
	return h.buildOpenCodeSummaryMapForAccount(accountID)
}

func (h *Handler) buildOpenCodeSummaryMapForAccount(accountID int64) map[string]interface{} {
	if h.store == nil || h.opencodeTracker == nil {
		return map[string]interface{}{}
	}

	quotaNames, err := h.store.QueryAllOpenCodeQuotaNamesForAccount(accountID)
	if err != nil {
		h.logger.Error("failed to query OpenCode quota names", "error", err)
		return map[string]interface{}{}
	}

	result := make(map[string]interface{})
	for _, name := range quotaNames {
		summary, err := h.opencodeTracker.UsageSummaryForAccount(accountID, name)
		if err != nil || summary == nil {
			continue
		}
		entry := map[string]interface{}{
			"currentUtil":     summary.CurrentUtil,
			"completedCycles": summary.CompletedCycles,
			"peakCycle":       summary.PeakCycle,
			"avgPerCycle":     summary.AvgPerCycle,
			"totalTracked":    summary.TotalTracked,
		}
		if summary.ResetsAt != nil {
			entry["resetsAt"] = summary.ResetsAt.Format(time.RFC3339)
			entry["timeUntilReset"] = formatDuration(summary.TimeUntilReset)
		}
		result[name] = entry
	}
	return result
}

func (h *Handler) buildOpenCodeInsights(hidden map[string]bool, _ time.Duration) opencodeInsightsResponse {
	if h.store == nil {
		return opencodeInsightsResponse{Stats: []opencodeInsightStat{}, Insights: []insightItem{}}
	}
	accountID, err := h.store.DefaultOpenCodeAccountID()
	if err != nil {
		return opencodeInsightsResponse{Stats: []opencodeInsightStat{}, Insights: []insightItem{}}
	}
	return h.buildOpenCodeInsightsForAccount(accountID, hidden, 0)
}

func (h *Handler) buildOpenCodeInsightsForAccount(accountID int64, hidden map[string]bool, _ time.Duration) opencodeInsightsResponse {
	resp := opencodeInsightsResponse{Stats: []opencodeInsightStat{}, Insights: []insightItem{}}

	if h.store == nil {
		return resp
	}

	latest, err := h.store.QueryLatestOpenCodeForAccount(accountID)
	if err != nil || latest == nil || len(latest.Quotas) == 0 {
		return resp
	}

	planLabel := latest.PlanName
	if planLabel == "" {
		planLabel = string(latest.AccountType)
	}
	if planLabel != "" {
		resp.Stats = append(resp.Stats, opencodeInsightStat{
			Label: "Plan",
			Value: planLabel,
		})
	}

	quotas := append([]api.OpenCodeQuota(nil), latest.Quotas...)
	sort.SliceStable(quotas, func(i, j int) bool {
		left := opencodeQuotaOrder(quotas[i].Name)
		right := opencodeQuotaOrder(quotas[j].Name)
		if left != right {
			return left < right
		}
		return quotas[i].Name < quotas[j].Name
	})

	summaries := map[string]*tracker.OpenCodeSummary{}
	if h.opencodeTracker != nil {
		for _, quota := range quotas {
			summary, err := h.opencodeTracker.UsageSummaryForAccount(accountID, quota.Name)
			if err == nil && summary != nil {
				summaries[quota.Name] = summary
			}
		}
	}

	preferredQuotas := []string{"five_hour", "weekly", "monthly"}
	selected := make([]api.OpenCodeQuota, 0, len(preferredQuotas))
	for _, name := range preferredQuotas {
		for _, quota := range quotas {
			if quota.Name == name {
				selected = append(selected, quota)
				break
			}
		}
	}
	if len(selected) == 0 {
		selected = quotas
	}

	for _, quota := range selected {
		rate := h.computeOpenCodeRateForAccount(accountID, quota.Name, quota.Utilization, summaries[quota.Name])
		insightKey := fmt.Sprintf("forecast_%s", quota.Name)
		if hidden[insightKey] {
			continue
		}
		value := "Analyzing..."
		if rate.HasRate {
			value = fmt.Sprintf("%.1f%%/hr", rate.Rate)
		}
		insight := buildOpenCodeBurnRateInsight(quota, rate)
		resp.Stats = append(resp.Stats, opencodeInsightStat{
			Key:      insightKey,
			Label:    fmt.Sprintf("%s Burn Rate", opencodeDisplayName(quota.Name)),
			Value:    value,
			Sublabel: insight.Sublabel,
			Metric:   insight.Metric,
			Severity: insight.Severity,
			Desc:     insight.Desc,
		})
	}

	return resp
}

func (h *Handler) loggingHistoryOpenCode(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"logs": []interface{}{}})
		return
	}

	start, end, limit := h.loggingHistoryRangeAndLimit(r)
	accountID, accountErr := h.openCodeAccountID(r)
	if accountErr != nil {
		respondError(w, http.StatusBadRequest, accountErr.Error())
		return
	}
	snapshots, err := h.store.QueryOpenCodeRangeForAccount(accountID, start, end, limit)
	if err != nil {
		h.logger.Error("failed to query OpenCode snapshots", "error", err)
		respondError(w, http.StatusInternalServerError, "failed to query logging history")
		return
	}

	quotaSet := map[string]bool{}
	for _, snap := range snapshots {
		for _, q := range snap.Quotas {
			quotaSet[q.Name] = true
		}
	}

	quotaNames := make([]string, 0, len(quotaSet))
	for qn := range quotaSet {
		quotaNames = append(quotaNames, qn)
	}
	if len(quotaNames) == 0 {
		quotaNames = []string{"five_hour", "weekly", "monthly"}
	} else {
		sort.SliceStable(quotaNames, func(i, j int) bool {
			left := opencodeQuotaOrder(quotaNames[i])
			right := opencodeQuotaOrder(quotaNames[j])
			if left != right {
				return left < right
			}
			return quotaNames[i] < quotaNames[j]
		})
	}

	capturedAt := make([]time.Time, 0, len(snapshots))
	ids := make([]int64, 0, len(snapshots))
	series := make([]map[string]loggingHistoryCrossQuota, 0, len(snapshots))

	for _, snap := range snapshots {
		capturedAt = append(capturedAt, snap.CapturedAt)
		ids = append(ids, snap.ID)
		row := make(map[string]loggingHistoryCrossQuota, len(snap.Quotas))
		for _, q := range snap.Quotas {
			row[q.Name] = loggingHistoryCrossQuota{
				Name:     q.Name,
				Value:    q.Used,
				Limit:    q.Limit,
				Percent:  q.Utilization,
				HasValue: q.Used > 0 || q.Limit > 0,
				HasLimit: q.Limit > 0,
			}
		}
		series = append(series, row)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"provider":   "opencode",
		"quotaNames": quotaNames,
		"logs":       loggingHistoryRowsFromSnapshots(capturedAt, ids, quotaNames, series),
	})
}
