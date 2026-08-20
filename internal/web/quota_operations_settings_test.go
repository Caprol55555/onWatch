package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onllm-dev/onwatch/v2/internal/config"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func TestSettingsExposeVersionBuildTimeAndPersistPaceSchedule(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.SetVersion("3.4.5")
	h.SetBuildTime("2026-08-19T12:34:56Z")

	update := `{"notifications":{"warning_threshold":80,"critical_threshold":95,"notify_warning":true,"notify_critical":true,"notify_reset":true,"cooldown_minutes":30,"pace":{"enabled":true,"warning_threshold":12,"critical_threshold":24,"workday_start":"09:00","workday_end":"18:00","lunch_start":"12:00","lunch_minutes":60,"workdays_per_week":5}}}`
	rr := httptest.NewRecorder()
	h.UpdateSettings(rr, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(update)))
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.GetSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	body := rr.Body.String()
	for _, expected := range []string{`"current_version":"3.4.5"`, `"build_time":"2026-08-19T12:34:56Z"`, `"warning_threshold":12`, `"workday_start":"09:00"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("settings missing %s: %s", expected, body)
		}
	}
}

func TestSettingsRejectInvalidPaceSchedule(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	body := `{"notifications":{"warning_threshold":80,"critical_threshold":95,"cooldown_minutes":30,"pace":{"enabled":true,"warning_threshold":20,"critical_threshold":10,"workday_start":"18:00","workday_end":"09:00","lunch_start":"12:00","lunch_minutes":60,"workdays_per_week":5}}}`
	rr := httptest.NewRecorder()
	h.UpdateSettings(rr, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContainerUpdateUsesHostTriggerWithoutExecutingCommands(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	h.SetVersion("2.13.3-caprol.abcdef0")
	requestPath := filepath.Join(t.TempDir(), "update-request.json")
	h.SetUpdateRequestPath(requestPath)

	rr := httptest.NewRecorder()
	h.CheckUpdate(rr, httptest.NewRequest(http.MethodGet, "/api/update/check", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"host_managed":true`) {
		t.Fatalf("check=%d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ApplyUpdate(rr, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("apply=%d %s", rr.Code, rr.Body.String())
	}
	payload, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"current_version":"2.13.3-caprol.abcdef0"`) || !strings.Contains(string(payload), `"backup_required":true`) || !strings.Contains(string(payload), `"rollback_on_failure":true`) || !strings.Contains(string(payload), `"healthcheck_path":"/api/update/status"`) || strings.Contains(string(payload), "docker") {
		t.Fatalf("unsafe or incomplete update request: %s", payload)
	}
}

func TestContainerUpdateDoesNotOverwriteQueuedHostRequest(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, nil, nil, &config.Config{})
	requestPath := filepath.Join(t.TempDir(), "update-request.json")
	h.SetUpdateRequestPath(requestPath)
	original := []byte(`{"queued":"keep-existing-request"}`)
	if err := os.WriteFile(requestPath, original, 0600); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ApplyUpdate(rr, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("apply=%d %s", rr.Code, rr.Body.String())
	}
	payload, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(original) {
		t.Fatalf("queued host request was overwritten: %s", payload)
	}
}
