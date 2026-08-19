package web

import (
	"strings"
	"testing"
)

func TestSettingsUseAutoSaveWithoutGlobalSaveButton(t *testing.T) {
	t.Parallel()

	settings := readEmbeddedFile(t, "templates/settings.html")
	if strings.Contains(settings, `id="settings-save-btn"`) {
		t.Fatal("settings page must not render the legacy global save button")
	}
	if !strings.Contains(settings, `id="settings-feedback"`) || !strings.Contains(settings, `aria-live="polite"`) {
		t.Fatal("settings page must retain an accessible auto-save status region")
	}

	appJS := readStaticAppJS(t)
	for _, marker := range []string{
		"setupSettingsAutoSave()",
		"function scheduleSettingsAutoSave",
		"async function saveSettingsAutomatically",
		"settingsAutoSaveInFlight",
		"clearTransientSettingsSecrets(settings)",
		"event.target.id === 'smtp-password'",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("settings auto-save integration missing %q", marker)
		}
	}
	if strings.Contains(appJS, "function setupSettingsSave()") {
		t.Fatal("legacy click-to-save handler must be removed")
	}
}

func TestProviderAccountCreationUsesNestedDialog(t *testing.T) {
	t.Parallel()

	settings := readEmbeddedFile(t, "templates/settings.html")
	for _, marker := range []string{
		`id="provider-account-editor-modal"`,
		`id="provider-account-editor-body"`,
		`id="provider-account-editor-save"`,
	} {
		if !strings.Contains(settings, marker) {
			t.Errorf("nested provider account dialog missing %s", marker)
		}
	}

	appJS := readStaticAppJS(t)
	for _, marker := range []string{
		"openProviderAccountEditor({",
		"function openOpenCodeAccountCreateDialog",
		"function openMiniMaxAccountCreateDialog",
		"closeProviderAccountEditor()",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("provider account editor integration missing %q", marker)
		}
	}
	if strings.Contains(appJS, `id="opencode-add-form"`) || strings.Contains(appJS, `id="minimax-add-form"`) {
		t.Fatal("account creation forms must not be appended inline to account lists")
	}
}

func TestOpenCodeAccountActionsStayOnOneLine(t *testing.T) {
	t.Parallel()

	appJS := readStaticAppJS(t)
	for _, className := range []string{"opencode-account-summary", "opencode-account-meta", "opencode-account-actions"} {
		if !strings.Contains(appJS, className) {
			t.Errorf("OpenCode account rows missing class %q", className)
		}
	}

	css := readEmbeddedFile(t, "static/style.css")
	if !strings.Contains(css, ".opencode-account-actions") || !strings.Contains(css, "white-space: nowrap") {
		t.Fatal("OpenCode account action buttons must be protected from text wrapping")
	}
}
