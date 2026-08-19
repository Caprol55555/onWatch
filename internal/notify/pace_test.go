package notify

import (
	"math"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/store"
)

func TestQuotaAlertMetricUsesWorkProgressForWeeklyAndMonthlyQuotas(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	cfg := PaceConfig{Enabled: true, WorkdayStart: "09:00", LunchStart: "12:00", LunchMinutes: 60, WorkdayEnd: "18:00", WorkdaysPerWeek: 5}
	weeklyReset := time.Date(2026, 8, 24, 9, 0, 0, 0, loc)
	// Monday 09:00 -> next Monday 09:00. Wednesday 14:00 has completed two
	// workdays plus four working hours (09-12 and 13-14): 2.5 / 5 = 50%.
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, loc)
	metric, pace, ok := quotaAlertMetric(QuotaStatus{QuotaKey: "weekly", Utilization: 65, ResetsAt: &weeklyReset}, cfg, now)
	if !ok || math.Abs(pace-50) > 0.01 || math.Abs(metric-15) > 0.01 {
		t.Fatalf("weekly metric=%v pace=%v ok=%v", metric, pace, ok)
	}

	monthlyReset := time.Date(2026, 9, 1, 9, 0, 0, 0, loc)
	metric, pace, ok = quotaAlertMetric(QuotaStatus{QuotaKey: "monthly", Utilization: 70, ResetsAt: &monthlyReset}, cfg, now)
	if !ok || pace <= 0 || pace >= 100 || math.Abs(metric-(70-pace)) > 0.01 {
		t.Fatalf("monthly metric=%v pace=%v ok=%v", metric, pace, ok)
	}
}

func TestQuotaAlertMetricFallsBackForShortWindowOrMissingReset(t *testing.T) {
	cfg := PaceConfig{Enabled: true, WorkdayStart: "09:00", LunchStart: "12:00", LunchMinutes: 60, WorkdayEnd: "18:00", WorkdaysPerWeek: 5}
	now := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	for _, status := range []QuotaStatus{{QuotaKey: "five_hour", Utilization: 81}, {QuotaKey: "weekly", Utilization: 82}} {
		metric, _, ok := quotaAlertMetric(status, cfg, now)
		if ok || metric != status.Utilization {
			t.Fatalf("fallback for %+v = metric %v ok %v", status, metric, ok)
		}
	}
}

func TestWorkdayProgressExcludesLunchAndNonWorkingDays(t *testing.T) {
	loc := time.UTC
	cfg := PaceConfig{Enabled: true, WorkdayStart: "09:00", LunchStart: "12:00", LunchMinutes: 60, WorkdayEnd: "18:00", WorkdaysPerWeek: 5}
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, loc) // Monday
	end := time.Date(2026, 8, 24, 9, 0, 0, 0, loc)
	for _, tc := range []struct {
		now  time.Time
		want float64
	}{
		{time.Date(2026, 8, 17, 8, 0, 0, 0, loc), 0},
		{time.Date(2026, 8, 17, 12, 30, 0, 0, loc), 3.0 / 40.0 * 100},
		{time.Date(2026, 8, 17, 18, 30, 0, 0, loc), 20},
		{time.Date(2026, 8, 22, 12, 0, 0, 0, loc), 100},
	} {
		got := workProgressPercent(start, end, tc.now, cfg)
		if math.Abs(got-tc.want) > 0.01 {
			t.Fatalf("at %v got %.3f want %.3f", tc.now, got, tc.want)
		}
	}
}

func TestQuotaAlertMetricUsesConfiguredTimezone(t *testing.T) {
	cfg := PaceConfig{Enabled: true, Warning: 10, Critical: 20, WorkdayStart: "09:00", LunchStart: "12:00", LunchMinutes: 60, WorkdayEnd: "18:00", WorkdaysPerWeek: 5, Timezone: "Asia/Shanghai"}
	// Reset is serialized as UTC, while the work schedule is configured in China time.
	resetUTC := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC) // Monday 09:00 CST
	nowUTC := time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC)   // Wednesday 14:00 CST
	metric, progress, ok := quotaAlertMetric(QuotaStatus{QuotaKey: "weekly", Utilization: 65, ResetsAt: &resetUTC}, cfg, nowUTC)
	if !ok || math.Abs(progress-50) > 0.01 || math.Abs(metric-15) > 0.01 {
		t.Fatalf("metric=%v progress=%v ok=%v", metric, progress, ok)
	}
}

func TestReloadPaceConfigKeepsDashboardTimezone(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting("timezone", "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("notifications", `{"warning_threshold":80,"critical_threshold":95,"notify_warning":true,"notify_critical":true,"pace":{"enabled":true,"warning_threshold":10,"critical_threshold":20,"workday_start":"09:00","workday_end":"18:00","lunch_start":"12:00","lunch_minutes":60,"workdays_per_week":5}}`); err != nil {
		t.Fatal(err)
	}
	engine := New(s, nil)
	if err := engine.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := engine.Config().Pace.Timezone; got != "Asia/Shanghai" {
		t.Fatalf("timezone=%q", got)
	}
}

func TestNotificationEngineCheckAppliesPaceOnlyToLongPeriodQuotas(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	engine := newTestEngine(t, s)
	if err := engine.Reload(); err != nil {
		t.Fatal(err)
	}
	mailCount, cleanup := setupSMTPAndMailer(t, s, engine)
	defer cleanup()

	// The short-period quota stays below the normal 80% warning threshold.
	engine.Check(QuotaStatus{Provider: "opencode", QuotaKey: "five_hour", Utilization: 25, Limit: 100})
	if got := mailCount.Load(); got != 0 {
		t.Fatalf("short-period quota unexpectedly sent %d notifications", got)
	}

	// Start the weekly period one minute ago. Its work progress is effectively
	// zero, so 25% utilization is more than 20 percentage points ahead and must
	// trigger a critical pace alert even though it is below the normal threshold.
	resetAt := time.Now().Add(7*24*time.Hour - time.Minute)
	engine.Check(QuotaStatus{Provider: "opencode", AccountID: "pace-test", QuotaKey: "weekly", Utilization: 25, Limit: 100, ResetsAt: &resetAt})
	if got := mailCount.Load(); got != 1 {
		t.Fatalf("weekly pace alert sent %d notifications, want 1", got)
	}
}
