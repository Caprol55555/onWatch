package main

import (
	"testing"
	"time"
)

func TestEffectiveBuildTimeUsesInjectedValueAndExecutableFallback(t *testing.T) {
	const injected = "2026-08-20T01:02:03Z"
	if got := effectiveBuildTime(injected); got != injected {
		t.Fatalf("injected build time=%q", got)
	}

	got := effectiveBuildTime("")
	if got == "" {
		t.Fatal("executable modification time should provide a build-time fallback")
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("fallback build time %q is not RFC3339: %v", got, err)
	}
}
