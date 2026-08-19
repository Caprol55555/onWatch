package store

import (
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
)

func TestQueryOpenCodeAggregateHistoryUsesLatestAccountSamplePerBucket(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, _ := s.CreateOpenCodeAccount("A", "aggregate-history-a", "cookie-a", true)
	b, _ := s.CreateOpenCodeAccount("B", "aggregate-history-b", "cookie-b", true)
	base := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	insert := func(accountID int64, capturedAt time.Time, utilization float64) {
		t.Helper()
		_, err := s.InsertOpenCodeSnapshotForAccount(accountID, &api.OpenCodeSnapshot{
			CapturedAt: capturedAt,
			Quotas:     []api.OpenCodeQuota{{Name: "weekly", Utilization: utilization, Format: api.OpenCodeQuotaFormatPercent}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(a.AccountID, base.Add(time.Minute), 10)
	insert(a.AccountID, base.Add(4*time.Minute), 20)
	insert(b.AccountID, base.Add(2*time.Minute), 60)

	points, err := s.QueryOpenCodeAggregateHistory(base, base.Add(time.Hour), 5*time.Minute, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("points=%+v, want one aggregate bucket", points)
	}
	if points[0].QuotaName != "weekly" || points[0].AverageUtilization != 40 || points[0].SampleCount != 2 {
		t.Fatalf("aggregate=%+v, want latest-per-account average 40 from two accounts", points[0])
	}
}

func TestQueryOpenCodeAggregateHistoryExcludesDisabledAndDeletedAccounts(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	active, _ := s.CreateOpenCodeAccount("Active", "aggregate-active", "cookie-a", true)
	disabled, _ := s.CreateOpenCodeAccount("Disabled", "aggregate-disabled", "cookie-b", false)
	deleted, _ := s.CreateOpenCodeAccount("Deleted", "aggregate-deleted", "cookie-c", true)
	if err := s.DeleteOpenCodeAccount(deleted.AccountID); err != nil {
		t.Fatal(err)
	}
	capturedAt := time.Now().UTC().Add(-time.Minute)
	for _, sample := range []struct {
		accountID   int64
		utilization float64
	}{{active.AccountID, 20}, {disabled.AccountID, 60}, {deleted.AccountID, 90}} {
		_, err := s.InsertOpenCodeSnapshotForAccount(sample.accountID, &api.OpenCodeSnapshot{
			CapturedAt: capturedAt,
			Quotas:     []api.OpenCodeQuota{{Name: "weekly", Utilization: sample.utilization}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	points, err := s.QueryOpenCodeAggregateHistory(capturedAt.Add(-time.Minute), capturedAt.Add(time.Minute), 5*time.Minute, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].AverageUtilization != 20 || points[0].SampleCount != 1 {
		t.Fatalf("aggregate includes disabled or deleted accounts: %+v", points)
	}
}

func TestQueryOpenCodeAggregateHistoryLimitsTimeBuckets(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	account, _ := s.CreateOpenCodeAccount("Bounded", "aggregate-bounded", "cookie", true)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 205; i++ {
		_, err := s.InsertOpenCodeSnapshotForAccount(account.AccountID, &api.OpenCodeSnapshot{
			CapturedAt: base.Add(time.Duration(i) * time.Minute),
			Quotas:     []api.OpenCodeQuota{{Name: "weekly", Utilization: float64(i % 100)}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	points, err := s.QueryOpenCodeAggregateHistory(base.Add(-time.Minute), base.Add(206*time.Minute), time.Minute, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 200 {
		t.Fatalf("points=%d, want 200 bounded buckets", len(points))
	}
	if !points[0].CapturedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("oldest retained bucket=%s, want %s", points[0].CapturedAt, base.Add(5*time.Minute))
	}
}
