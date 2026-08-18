package config

import (
	"strings"
	"testing"
)

func TestConfigOpenCodeProviderEnabledByEncryptedDatabaseAccounts(t *testing.T) {
	cfg := &Config{OpenCodeAccountsConfigured: true}
	if !cfg.HasProvider("opencode") {
		t.Fatal("encrypted database accounts should enable OpenCode without legacy env credentials")
	}
	if got := cfg.AvailableProviders(); !containsString(got, "opencode") {
		t.Fatalf("AvailableProviders() = %v", got)
	}
}

func TestConfigStringNeverIncludesOpenCodeCookieFragments(t *testing.T) {
	cookie := "auth-cookie-visible-prefix-and-suffix"
	got := (&Config{OpenCodeGoAuthCookie: cookie}).String()
	for _, fragment := range []string{cookie, "auth", "suffix"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("Config.String leaked OpenCode Cookie fragment %q: %s", fragment, got)
		}
	}
	if !strings.Contains(got, "OpenCodeGoAuthCookie: configured") {
		t.Fatalf("Config.String did not report configured state: %s", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
