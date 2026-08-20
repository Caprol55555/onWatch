package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dbStatus, err := h.store.MaintenanceStatus()
	if err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]any{"healthy": false, "sqlite": false})
		return
	}
	running, expected := 0, 0
	collectorsHealthy := true
	visibility := h.providerVisibilityMap()
	for _, provider := range []string{"synthetic", "zai", "anthropic", "copilot", "codex", "antigravity", "minimax", "openrouter", "gemini", "cursor", "grok", "kimi", "moonshot", "deepseek", "opencode"} {
		if h.isProviderConfigured(provider) && h.providerPollingEnabled(provider, visibility) {
			expected++
			if h.agentManager == nil || !h.agentManager.IsRunning(provider) {
				collectorsHealthy = false
			}
		}
		if h.agentManager != nil && h.agentManager.IsRunning(provider) {
			running++
		}
	}
	sqliteHealthy := h.store.CheckHealth() == nil
	result := map[string]any{"healthy": sqliteHealthy && collectorsHealthy, "http": true, "sqlite": sqliteHealthy, "schema": h.store.SchemaVersion() > 0, "schema_version": h.store.SchemaVersion(), "database_bytes": dbStatus.DatabaseBytes, "collectors_running": running, "collectors_expected": expected, "collectors_healthy": collectorsHealthy, "current_version": h.version, "build_time": h.buildTime}
	if h.updateRequestPath != "" {
		resultPath := filepath.Clean(h.updateRequestPath) + ".result.json"
		if info, statErr := os.Stat(resultPath); statErr == nil && info.Size() <= 32*1024 {
			if payload, readErr := os.ReadFile(resultPath); readErr == nil {
				// Whitelist host-consumer fields instead of reflecting an arbitrary
				// local JSON object through the authenticated API.
				var hostResult struct {
					Status          string `json:"status"`
					RequestedAt     string `json:"requested_at,omitempty"`
					CompletedAt     string `json:"completed_at,omitempty"`
					PreviousVersion string `json:"previous_version,omitempty"`
					CurrentVersion  string `json:"current_version,omitempty"`
					ImageDigest     string `json:"image_digest,omitempty"`
					RolledBack      bool   `json:"rolled_back,omitempty"`
				}
				if json.Unmarshal(payload, &hostResult) == nil {
					result["last_update"] = hostResult
				}
			}
		}
	}
	respondJSON(w, http.StatusOK, result)
}

func (h *Handler) AlertAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID      int64  `json:"id"`
		Action  string `json:"action"`
		Minutes int    `json:"minutes"`
	}
	if decodeStrictJSON(http.MaxBytesReader(w, r.Body, 4096), &body) != nil || body.ID <= 0 {
		respondError(w, http.StatusBadRequest, "invalid alert action")
		return
	}
	var err error
	switch body.Action {
	case "acknowledge":
		err = h.store.AcknowledgeSystemAlert(body.ID)
	case "resolve":
		err = h.store.ResolveSystemAlert(body.ID)
	case "silence":
		minutes := body.Minutes
		if minutes <= 0 {
			minutes = h.store.AlertLifecyclePolicy().SilenceMinutes
		}
		if minutes > 10080 {
			minutes = 10080
		}
		err = h.store.SilenceSystemAlert(body.ID, time.Now().Add(time.Duration(minutes)*time.Minute))
	default:
		respondError(w, http.StatusBadRequest, "invalid alert action")
		return
	}
	if err != nil {
		h.logger.Error("alert lifecycle update failed", "alert_id", strconv.FormatInt(body.ID, 10), "error", err)
		respondError(w, http.StatusInternalServerError, "alert update failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": body.Action})
}

// MaintenanceStatus returns bounded database statistics and the active policy.
func (h *Handler) MaintenanceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	status, err := h.store.MaintenanceStatus()
	if err != nil {
		h.logger.Error("maintenance status failed", "error", err)
		respondError(w, http.StatusInternalServerError, "maintenance status failed")
		return
	}
	backups, err := h.store.ListBackups()
	if err != nil {
		h.logger.Error("backup list failed", "error", err)
		respondError(w, http.StatusInternalServerError, "backup list failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"database": status, "retention": h.store.RetentionPolicy(), "backups": backups})
}

func (h *Handler) CollectionHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	interval := 2 * time.Minute
	if h.config != nil && h.config.PollInterval > 0 {
		interval = h.config.PollInterval
	}
	items, err := h.store.QueryCollectionHealth(interval, time.Now().UTC())
	if err != nil {
		h.logger.Error("collection health query failed", "error", err)
		respondError(w, http.StatusInternalServerError, "collection health query failed")
		return
	}
	configured := make(map[string]bool)
	visibility := h.providerVisibilityMap()
	for i := range items {
		provider := items[i].Provider
		isConfigured, ok := configured[provider]
		if !ok {
			isConfigured = h.isProviderConfigured(provider)
			configured[provider] = isConfigured
		}
		items[i].Configured = isConfigured
		enabled := h.providerPollingEnabled(provider, visibility)
		if provider == "minimax" && items[i].AccountID > 0 {
			enabled = enabled && h.providerPollingEnabled(fmt.Sprintf("minimax:%d", items[i].AccountID), visibility)
		}
		if provider == "opencode" {
			enabled = enabled && items[i].Enabled
		}
		items[i].Enabled = enabled
		if !enabled {
			items[i].Status = "disabled"
		} else if isConfigured && h.agentManager != nil && !h.agentManager.IsRunning(provider) && items[i].Status == "healthy" {
			items[i].Status = "stopped"
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "poll_interval_seconds": int(interval / time.Second)})
}

// RunMaintenance creates a safety backup, prunes one bounded batch, and asks
// SQLite to checkpoint WAL pages without blocking readers for a full vacuum.
func (h *Handler) RunMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	backup, err := h.store.CreateBackup(store.BackupReasonManual)
	if err != nil {
		h.logger.Error("maintenance backup failed", "error", err)
		respondError(w, http.StatusInternalServerError, "maintenance backup failed")
		return
	}
	report, err := h.store.RunRetention(h.store.RetentionPolicy(), time.Now())
	if err != nil {
		_ = h.store.DeleteBackup(backup.Name)
		h.logger.Error("retention run failed", "error", err)
		respondError(w, http.StatusInternalServerError, "retention run failed")
		return
	}
	if err = h.store.Checkpoint(); err != nil {
		h.logger.Warn("maintenance checkpoint failed", "error", err)
	}
	if err = h.store.MarkMaintenanceCompleted(time.Now()); err != nil {
		h.logger.Warn("maintenance completion timestamp failed", "error", err)
	}
	deletedBackups, pruneErr := h.store.PruneBackups(h.store.RetentionPolicy().BackupDays, time.Now())
	if pruneErr != nil {
		h.logger.Warn("backup retention failed", "error", pruneErr)
	}
	respondJSON(w, http.StatusOK, map[string]any{"report": report, "backup": backup, "deleted_backups": deletedBackups})
}

// Backups lists, creates, or deletes administrator-managed SQLite snapshots.
func (h *Handler) Backups(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := h.store.ListBackups()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "backup list failed")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"backups": items})
	case http.MethodPost:
		item, err := h.store.CreateBackup(store.BackupReasonManual)
		if err != nil {
			h.logger.Error("backup creation failed", "error", err)
			respondError(w, http.StatusInternalServerError, "backup creation failed")
			return
		}
		respondJSON(w, http.StatusCreated, map[string]any{"backup": item})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := h.store.DeleteBackup(name); err != nil {
			respondError(w, http.StatusBadRequest, "invalid backup")
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) RestoreBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if decodeStrictJSON(r.Body, &body) != nil || strings.TrimSpace(body.Name) == "" {
		respondError(w, http.StatusBadRequest, "invalid backup")
		return
	}
	if err := h.store.StageRestore(strings.TrimSpace(body.Name)); err != nil {
		h.logger.Warn("backup restore staging rejected", "error", err)
		respondError(w, http.StatusBadRequest, "backup verification failed")
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]any{"status": "restore_staged", "restart_required": true})
}

func (h *Handler) DownloadBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	file, size, err := h.store.OpenBackup(name)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid backup")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, file)
}

// RetryCollection restarts one bounded provider runner. Account-specific
// managers retain their own concurrency and retry limits.
func (h *Handler) RetryCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.agentManager == nil {
		respondError(w, http.StatusServiceUnavailable, "agent manager not available")
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if decodeStrictJSON(http.MaxBytesReader(w, r.Body, 4096), &body) != nil {
		respondError(w, http.StatusBadRequest, "invalid request")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	_, allowed := map[string]struct{}{
		"synthetic": {}, "zai": {}, "anthropic": {}, "copilot": {}, "codex": {}, "antigravity": {},
		"minimax": {}, "openrouter": {}, "gemini": {}, "cursor": {}, "grok": {}, "kimi": {},
		"moonshot": {}, "deepseek": {}, "opencode": {}, "api_integrations": {},
	}[provider]
	if !allowed {
		respondError(w, http.StatusBadRequest, "invalid provider")
		return
	}
	if provider != "api_integrations" && !h.isProviderConfigured(provider) {
		respondError(w, http.StatusConflict, "provider is not configured")
		return
	}
	if !h.providerPollingEnabled(provider, h.providerVisibilityMap()) {
		respondError(w, http.StatusConflict, "provider polling is disabled")
		return
	}
	h.agentManager.Stop(provider)
	if err := h.agentManager.Start(provider); err != nil {
		h.logger.Warn("manual collection retry failed", "provider", provider, "error", err)
		respondError(w, http.StatusConflict, "provider restart failed")
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]string{"status": "retry_started", "provider": provider})
}
