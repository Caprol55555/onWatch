package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/config"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func disableAllProviderPollingForTest(t *testing.T, h *Handler) {
	t.Helper()
	value := false
	for _, provider := range providerCatalog() {
		if err := h.setProviderVisibility(provider.Key, &value, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOperationsSettingsRoundTripAndSecretsStayOut(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "onwatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	disableAllProviderPollingForTest(t, h)
	body := `{"retention":{"snapshot_days":45,"cycle_days":400,"alert_days":30,"backup_days":14,"batch_size":800},"alerts":{"failure_confirmations":3,"recovery_confirmations":2,"silence_minutes":60}}`
	rr := httptest.NewRecorder()
	h.UpdateSettings(rr, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.GetSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"snapshot_days":45`) || !strings.Contains(rr.Body.String(), `"failure_confirmations":3`) {
		t.Fatalf("settings response=%s", rr.Body.String())
	}
}

func TestMaintenanceAndBackupHandlers(t *testing.T) {
	dbDir := t.TempDir()
	s, err := store.New(filepath.Join(dbDir, "onwatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})

	rr := httptest.NewRecorder()
	h.MaintenanceStatus(rr, httptest.NewRequest(http.MethodGet, "/api/maintenance", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "database_bytes") {
		t.Fatalf("status response=%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "backup_directory") || strings.Contains(rr.Body.String(), dbDir) {
		t.Fatalf("maintenance response disclosed a filesystem path: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.Backups(rr, httptest.NewRequest(http.MethodPost, "/api/backups", nil))
	if rr.Code != http.StatusCreated {
		t.Fatalf("backup status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Backup store.BackupInfo `json:"backup"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Backup.Name == "" {
		t.Fatal("backup name missing")
	}

	rr = httptest.NewRecorder()
	h.Backups(rr, httptest.NewRequest(http.MethodGet, "/api/backups", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), created.Backup.Name) {
		t.Fatalf("list=%s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.RestoreBackup(rr, httptest.NewRequest(http.MethodPost, "/api/backups/restore", strings.NewReader(`{"name":"`+created.Backup.Name+`"}`)))
	if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), "restart_required") {
		t.Fatalf("restore=%d %s", rr.Code, rr.Body.String())
	}
}

func TestProviderConnectionResponseUsesStableContract(t *testing.T) {
	s, _ := store.New(":memory:")
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.providerCredentialConnectionTest = func(_ context.Context, _ string, _ map[string]string) error { return nil }
	rr := httptest.NewRecorder()
	h.ProviderCredentialTest(rr, httptest.NewRequest(http.MethodPost, "/api/providers/test", strings.NewReader(`{"provider":"zai","settings":{"api_key":"secret","region":"cn"}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, field := range []string{`"connected":true`, `"authenticated":true`, `"quota_parsed":true`, `"stage":"complete"`} {
		if !strings.Contains(rr.Body.String(), field) {
			t.Fatalf("missing %s in %s", field, rr.Body.String())
		}
	}
	if strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("response leaked credential: %s", rr.Body.String())
	}
}

func TestProviderConnectionContractClassifiesFailureStage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"network", api.ErrNetworkError, []string{`"connected":false`, `"stage":"network"`}},
		{"authentication", api.ErrUnauthorized, []string{`"connected":true`, `"authenticated":false`, `"stage":"authentication"`}},
		{"quota parse", api.ErrInvalidResponse, []string{`"connected":true`, `"authenticated":true`, `"quota_parsed":false`, `"stage":"quota_parse"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := NewHandler(s, nil, nil, nil, &config.Config{})
			h.providerCredentialConnectionTest = func(_ context.Context, _ string, _ map[string]string) error { return tc.err }
			rr := httptest.NewRecorder()
			h.ProviderCredentialTest(rr, httptest.NewRequest(http.MethodPost, "/api/providers/test", strings.NewReader(`{"provider":"synthetic","settings":{"api_key":"secret"}}`)))
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"success":false`) {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			for _, field := range tc.want {
				if !strings.Contains(rr.Body.String(), field) {
					t.Fatalf("missing %s in %s", field, rr.Body.String())
				}
			}
			if strings.Contains(rr.Body.String(), "secret") {
				t.Fatalf("response leaked credential: %s", rr.Body.String())
			}
		})
	}
}

func TestAlertActionAndUpdateHealth(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "onwatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	disableAllProviderPollingForTest(t, h)
	id, _ := s.CreateSystemAlert("opencode", "auth_error", "title", "message", "warning", "")
	rr := httptest.NewRecorder()
	h.AlertAction(rr, httptest.NewRequest(http.MethodPost, "/api/alerts/action", strings.NewReader(`{"id":`+strconv.FormatInt(id, 10)+`,"action":"acknowledge"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("alert action=%d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.SystemAlerts(rr, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"acknowledged"`) {
		t.Fatalf("acknowledged alert response=%d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.AlertAction(rr, httptest.NewRequest(http.MethodPost, "/api/alerts/action", strings.NewReader(`{"id":`+strconv.FormatInt(id, 10)+`,"action":"resolve"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve action=%d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.SystemAlerts(rr, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"alerts":[]`) {
		t.Fatalf("resolved alert remained active=%d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.UpdateStatus(rr, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"healthy":true`) || !strings.Contains(rr.Body.String(), `"sqlite":true`) {
		t.Fatalf("health=%d %s visibility=%v", rr.Code, rr.Body.String(), h.providerVisibilityMap())
	}
}

func TestCollectionHealthMarksConfiguredProvidersAndRetryRejectsUnconfigured(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "onwatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	agents := &mockProviderAgentController{running: map[string]bool{}}
	h := NewHandler(s, nil, nil, nil, &config.Config{ZaiAPIKey: "configured"})
	h.agentManager = agents
	disableAllProviderPollingForTest(t, h)
	polling := false
	if err := h.setProviderVisibility("zai", &polling, nil); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.CollectionHealth(rr, httptest.NewRequest(http.MethodGet, "/api/collection/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("collection health status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Items []store.CollectionHealth `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	configured := map[string]bool{}
	status := map[string]string{}
	for _, item := range response.Items {
		configured[item.Provider] = item.Configured
		status[item.Provider] = item.Status
	}
	if !configured["zai"] {
		t.Fatal("configured Z.ai provider was not marked configured")
	}
	if configured["deepseek"] {
		t.Fatal("unconfigured DeepSeek provider was marked configured")
	}
	if status["zai"] != "disabled" {
		t.Fatalf("disabled Z.ai status=%q, want disabled", status["zai"])
	}

	rr = httptest.NewRecorder()
	h.RetryCollection(rr, httptest.NewRequest(http.MethodPost, "/api/collection/retry", strings.NewReader(`{"provider":"deepseek"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("unconfigured retry status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(agents.started) != 0 || len(agents.stopped) != 0 {
		t.Fatalf("unconfigured retry changed agent state: started=%v stopped=%v", agents.started, agents.stopped)
	}

	polling = true
	if err := h.setProviderVisibility("zai", &polling, nil); err != nil {
		t.Fatal(err)
	}
	agents.running["codex"] = true // An unrelated runner must not mask a stopped expected collector.
	rr = httptest.NewRecorder()
	h.UpdateStatus(rr, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"healthy":false`) || !strings.Contains(rr.Body.String(), `"collectors_expected":1`) {
		t.Fatalf("stopped configured collector health=%d %s", rr.Code, rr.Body.String())
	}
	agents.running["zai"] = true
	rr = httptest.NewRecorder()
	h.UpdateStatus(rr, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"healthy":true`) {
		t.Fatalf("running collector health=%d %s", rr.Code, rr.Body.String())
	}
}

func TestDownloadBackupReturnsVerifiedSQLiteWithoutPathDisclosure(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "onwatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup, err := s.CreateBackup(store.BackupReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, nil, nil, nil, &config.Config{})

	rr := httptest.NewRecorder()
	h.DownloadBackup(rr, httptest.NewRequest(http.MethodGet, "/api/backups/download?name="+backup.Name, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("download status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/vnd.sqlite3" {
		t.Fatalf("content type=%q", got)
	}
	if !strings.Contains(rr.Header().Get("Content-Disposition"), backup.Name) {
		t.Fatalf("content disposition=%q", rr.Header().Get("Content-Disposition"))
	}
	if strings.Contains(rr.Body.String(), filepath.Dir(backup.Path)) {
		t.Fatal("download disclosed the backup directory")
	}
	if rr.Body.Len() == 0 {
		t.Fatal("download body is empty")
	}
}
