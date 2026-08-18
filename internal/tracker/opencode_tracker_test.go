package tracker

import (
	"log/slog"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func newTestOpenCodeStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCodeTracker_Process_FirstSnapshot(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	resetsAt := now.Add(5 * time.Hour)

	snapshot := &api.OpenCodeSnapshot{
		CapturedAt:  now,
		AccountType: api.OpenCodeAccountTypePro,
		PlanName:    "OpenCode Go",
		Quotas: []api.OpenCodeQuota{
			{Name: "five_hour", Utilization: 12.5, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &resetsAt},
		},
	}

	if err := tr.Process(snapshot); err != nil {
		t.Fatalf("Process: %v", err)
	}

	cycle, err := s.QueryActiveOpenCodeCycle("five_hour")
	if err != nil {
		t.Fatalf("QueryActiveOpenCodeCycle: %v", err)
	}
	if cycle == nil {
		t.Fatal("expected active cycle after first snapshot")
	}
	if cycle.PeakUtilization != 12.5 {
		t.Errorf("PeakUtilization = %f, want 12.5", cycle.PeakUtilization)
	}
}

func TestOpenCodeTracker_Process_UsageIncrease(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	resetsAt := now.Add(5 * time.Hour)

	snap1 := &api.OpenCodeSnapshot{
		CapturedAt: now,
		Quotas: []api.OpenCodeQuota{
			{Name: "five_hour", Utilization: 12.5, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &resetsAt},
		},
	}
	if err := tr.Process(snap1); err != nil {
		t.Fatalf("Process snap1: %v", err)
	}

	snap2 := &api.OpenCodeSnapshot{
		CapturedAt: now.Add(time.Minute),
		Quotas: []api.OpenCodeQuota{
			{Name: "five_hour", Utilization: 25.0, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &resetsAt},
		},
	}
	if err := tr.Process(snap2); err != nil {
		t.Fatalf("Process snap2: %v", err)
	}

	cycle, err := s.QueryActiveOpenCodeCycle("five_hour")
	if err != nil {
		t.Fatalf("QueryActiveOpenCodeCycle: %v", err)
	}
	if cycle == nil {
		t.Fatal("expected active cycle")
	}
	if cycle.PeakUtilization != 25.0 {
		t.Errorf("PeakUtilization = %f, want 25.0", cycle.PeakUtilization)
	}
	if cycle.TotalDelta != 12.5 {
		t.Errorf("TotalDelta = %f, want 12.5", cycle.TotalDelta)
	}
}

func TestOpenCodeTracker_Process_ResetDetection(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	oldReset := now.Add(1 * time.Hour)
	newReset := now.Add(6 * time.Hour)

	snap1 := &api.OpenCodeSnapshot{
		CapturedAt: now,
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 40, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &oldReset},
		},
	}
	if err := tr.Process(snap1); err != nil {
		t.Fatalf("Process snap1: %v", err)
	}

	snap2 := &api.OpenCodeSnapshot{
		CapturedAt: now.Add(2 * time.Hour),
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 5, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &newReset},
		},
	}
	if err := tr.Process(snap2); err != nil {
		t.Fatalf("Process snap2: %v", err)
	}

	history, err := s.QueryOpenCodeCycleHistory("weekly")
	if err != nil {
		t.Fatalf("QueryOpenCodeCycleHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("completed cycles = %d, want 1", len(history))
	}

	active, err := s.QueryActiveOpenCodeCycle("weekly")
	if err != nil {
		t.Fatalf("QueryActiveOpenCodeCycle: %v", err)
	}
	if active == nil {
		t.Fatal("expected new active cycle after reset")
	}
}

func TestOpenCodeTracker_Process_IgnoresFallbackResetTimeDrift(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	resetsAt := now.Add(7 * 24 * time.Hour)
	snap1 := &api.OpenCodeSnapshot{
		CapturedAt: now,
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 30, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &resetsAt},
		},
	}
	if err := tr.Process(snap1); err != nil {
		t.Fatalf("Process snap1: %v", err)
	}

	driftedReset := resetsAt.Add(-59 * time.Minute)
	snap2 := &api.OpenCodeSnapshot{
		CapturedAt: now.Add(time.Minute),
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 31, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &driftedReset},
		},
	}
	if err := tr.Process(snap2); err != nil {
		t.Fatalf("Process snap2: %v", err)
	}

	history, err := s.QueryOpenCodeCycleHistory("weekly")
	if err != nil {
		t.Fatalf("QueryOpenCodeCycleHistory: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("completed cycles = %d, want 0", len(history))
	}
}

func TestOpenCodeTracker_Process_IgnoresExpiredStoredResetWithinDriftTolerance(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	storedReset := now.Add(time.Hour)
	snap1 := &api.OpenCodeSnapshot{
		CapturedAt: now,
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 30, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &storedReset},
		},
	}
	if err := tr.Process(snap1); err != nil {
		t.Fatalf("Process snap1: %v", err)
	}

	currentReset := storedReset.Add(59 * time.Minute)
	snap2 := &api.OpenCodeSnapshot{
		CapturedAt: storedReset.Add(3 * time.Minute),
		Quotas: []api.OpenCodeQuota{
			{Name: "weekly", Utilization: 31, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &currentReset},
		},
	}
	if err := tr.Process(snap2); err != nil {
		t.Fatalf("Process snap2: %v", err)
	}

	history, err := s.QueryOpenCodeCycleHistory("weekly")
	if err != nil {
		t.Fatalf("QueryOpenCodeCycleHistory: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("completed cycles = %d, want 0", len(history))
	}
}

func TestOpenCodeTracker_UsageSummary(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())

	now := time.Now().UTC()
	resetsAt := now.Add(5 * time.Hour)
	snap := &api.OpenCodeSnapshot{
		CapturedAt:  now,
		AccountType: api.OpenCodeAccountTypePro,
		PlanName:    "OpenCode Go",
		Quotas: []api.OpenCodeQuota{
			{Name: "five_hour", Utilization: 20, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &resetsAt},
		},
	}
	if _, err := s.InsertOpenCodeSnapshot(snap); err != nil {
		t.Fatalf("InsertOpenCodeSnapshot: %v", err)
	}
	if err := tr.Process(snap); err != nil {
		t.Fatalf("Process: %v", err)
	}

	summary, err := tr.UsageSummary("five_hour")
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	if summary == nil {
		t.Fatal("expected summary")
	}
	if summary.CurrentUtil != 20 {
		t.Errorf("CurrentUtil = %f, want 20", summary.CurrentUtil)
	}
}

func TestOpenCodeTracker_IsolatesSameQuotaAcrossAccounts(t *testing.T) {
	s := newTestOpenCodeStore(t)
	tr := NewOpenCodeTracker(s, slog.Default())
	a, err := s.CreateOpenCodeAccount("A", "ws-a", "cookie-a", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateOpenCodeAccount("B", "ws-b", "cookie-b", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reset := now.Add(7 * 24 * time.Hour)
	for _, sample := range []struct {
		accountID int64
		captured  time.Time
		util      float64
	}{
		{a.AccountID, now, 10},
		{b.AccountID, now, 80},
		{a.AccountID, now.Add(time.Minute), 20},
		{b.AccountID, now.Add(time.Minute), 85},
	} {
		snapshot := &api.OpenCodeSnapshot{CapturedAt: sample.captured, Quotas: []api.OpenCodeQuota{{Name: "weekly", Utilization: sample.util, Format: api.OpenCodeQuotaFormatPercent, ResetsAt: &reset}}}
		if err := tr.ProcessForAccount(sample.accountID, snapshot); err != nil {
			t.Fatalf("ProcessForAccount(%d): %v", sample.accountID, err)
		}
	}

	cycleA, err := s.QueryActiveOpenCodeCycleForAccount(a.AccountID, "weekly")
	if err != nil {
		t.Fatal(err)
	}
	cycleB, err := s.QueryActiveOpenCodeCycleForAccount(b.AccountID, "weekly")
	if err != nil {
		t.Fatal(err)
	}
	if cycleA == nil || cycleA.PeakUtilization != 20 || cycleA.TotalDelta != 10 {
		t.Fatalf("account A cycle mixed: %+v", cycleA)
	}
	if cycleB == nil || cycleB.PeakUtilization != 85 || cycleB.TotalDelta != 5 {
		t.Fatalf("account B cycle mixed: %+v", cycleB)
	}
}
