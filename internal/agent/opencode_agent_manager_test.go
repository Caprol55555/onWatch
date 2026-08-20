package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/store"
	"github.com/onllm-dev/onwatch/v2/internal/tracker"
)

type concurrentOpenCodeFetcher struct {
	mu        sync.Mutex
	active    int
	maxActive int
	failWS    string
}

type blockingOpenCodeFetcher struct {
	started     chan struct{}
	release     chan struct{}
	cancelDelay time.Duration
	finished    chan struct{}
}

func (f *blockingOpenCodeFetcher) FetchSnapshot(ctx context.Context, _, _ string) (*api.OpenCodeSnapshot, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		now := time.Now().UTC()
		return &api.OpenCodeSnapshot{CapturedAt: now, Quotas: []api.OpenCodeQuota{{Name: "weekly", Utilization: 10, Format: api.OpenCodeQuotaFormatPercent}}}, nil
	case <-ctx.Done():
		if f.cancelDelay > 0 {
			time.Sleep(f.cancelDelay)
		}
		if f.finished != nil {
			select {
			case f.finished <- struct{}{}:
			default:
			}
		}
		return nil, ctx.Err()
	}
}

func (f *concurrentOpenCodeFetcher) FetchSnapshot(_ context.Context, workspaceID, _ string) (*api.OpenCodeSnapshot, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	time.Sleep(20 * time.Millisecond)
	if workspaceID == f.failWS {
		return nil, api.ErrOpenCodeUnauthorized
	}
	now := time.Now().UTC()
	return &api.OpenCodeSnapshot{CapturedAt: now, Quotas: []api.OpenCodeQuota{{Name: "weekly", Utilization: 10, Format: api.OpenCodeQuotaFormatPercent}}}, nil
}

func TestOpenCodeAgentManagerBoundsConcurrencyAndIsolatesFailures(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var failedID int64
	for i, workspace := range []string{"ok-1", "bad", "ok-2", "ok-3"} {
		a, err := s.CreateOpenCodeAccount(workspace, workspace, "cookie", true)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			failedID = a.AccountID
		}
	}
	fetcher := &concurrentOpenCodeFetcher{failWS: "bad"}
	mgr := NewOpenCodeAgentManager(fetcher, s, tracker.NewOpenCodeTracker(s, slog.Default()), 120*time.Second, slog.Default())
	mgr.SetWorkerCount(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.pollDueAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgr.waitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if fetcher.maxActive > 2 {
		t.Fatalf("max concurrency = %d, want <= 2", fetcher.maxActive)
	}

	failed, err := s.GetOpenCodeAccount(failedID, false)
	if err != nil || failed.AuthStatus != store.OpenCodeAuthError || failed.ConsecutiveFailures != 1 {
		t.Fatalf("first auth failure should remain retryable: account=%+v err=%v", failed, err)
	}
	for _, workspace := range []string{"ok-1", "ok-2", "ok-3"} {
		accounts, _ := s.QueryOpenCodeAccounts(false)
		for _, account := range accounts {
			if account.WorkspaceID == workspace {
				latest, err := s.QueryLatestOpenCodeForAccount(account.AccountID)
				if err != nil || latest == nil {
					t.Fatalf("%s was blocked by another failure: latest=%+v err=%v", workspace, latest, err)
				}
			}
		}
	}
	if !errors.Is(api.ErrOpenCodeUnauthorized, api.ErrOpenCodeUnauthorized) {
		t.Fatal("sentinel sanity")
	}
}

func TestOpenCodeAgentManagerRequiresConsecutiveAuthFailuresBeforeReauth(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account, err := s.CreateOpenCodeAccount("A", "bad", "cookie", true)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewOpenCodeAgentManager(&concurrentOpenCodeFetcher{failWS: "bad"}, s, nil, time.Second, slog.Default())

	first, err := s.GetOpenCodeAccount(account.AccountID, false)
	if err != nil {
		t.Fatal(err)
	}
	mgr.handlePollError(*first, api.ErrOpenCodeUnauthorized)
	first, _ = s.GetOpenCodeAccount(account.AccountID, false)
	if first.AuthStatus != store.OpenCodeAuthError || first.ConsecutiveFailures != 1 {
		t.Fatalf("first auth failure must be retryable, got %+v", first)
	}

	mgr.handlePollError(*first, api.ErrOpenCodeUnauthorized)
	second, _ := s.GetOpenCodeAccount(account.AccountID, false)
	if second.AuthStatus != store.OpenCodeAuthNeedsReauth || second.ConsecutiveFailures != 2 {
		t.Fatalf("second consecutive auth failure must require reauth, got %+v", second)
	}
}

func TestOpenCodeAgentManagerResetsAuthConfirmationWhenFailureClassChanges(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting(store.SettingAlertLifecycle, `{"failure_confirmations":3,"recovery_confirmations":2,"silence_minutes":60}`); err != nil {
		t.Fatal(err)
	}
	account, err := s.CreateOpenCodeAccount("A", "changing-errors", "cookie", true)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewOpenCodeAgentManager(&concurrentOpenCodeFetcher{}, s, nil, time.Second, slog.Default())

	apply := func(pollErr error) *store.OpenCodeAccount {
		t.Helper()
		current, getErr := s.GetOpenCodeAccount(account.AccountID, false)
		if getErr != nil {
			t.Fatal(getErr)
		}
		mgr.handlePollError(*current, pollErr)
		updated, getErr := s.GetOpenCodeAccount(account.AccountID, false)
		if getErr != nil {
			t.Fatal(getErr)
		}
		return updated
	}

	apply(api.ErrOpenCodeUnauthorized)
	apply(api.ErrOpenCodeNetworkError)
	apply(api.ErrOpenCodeUnauthorized)
	fourth := apply(api.ErrOpenCodeUnauthorized)
	if fourth.AuthStatus == store.OpenCodeAuthNeedsReauth {
		t.Fatalf("only two consecutive auth failures after a network error must remain retryable: %+v", fourth)
	}
	fifth := apply(api.ErrOpenCodeUnauthorized)
	if fifth.AuthStatus != store.OpenCodeAuthNeedsReauth {
		t.Fatalf("third consecutive auth failure must require reauthentication: %+v", fifth)
	}
}

func TestOpenCodeNotificationStatusSeparatesAccountFromQuota(t *testing.T) {
	status := openCodeNotificationStatus(42, api.OpenCodeQuota{Name: "weekly", Utilization: 61.5, Limit: 100})
	if status.Provider != "opencode" || status.QuotaKey != "weekly" || status.AccountID != "42" {
		t.Fatalf("unexpected notification status: %+v", status)
	}
}

func TestOpenCodeAgentManagerDiscardsStaleCredentialResult(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account, err := s.CreateOpenCodeAccount("A", "ws-a", "old-cookie", true)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &blockingOpenCodeFetcher{started: make(chan struct{}, 1), release: make(chan struct{})}
	mgr := NewOpenCodeAgentManager(fetcher, s, nil, 120*time.Second, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.pollDueAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	newCookie := "new-cookie"
	if _, err := s.UpdateOpenCodeAccount(account.AccountID, "A", "ws-a", &newCookie, true); err != nil {
		t.Fatal(err)
	}
	close(fetcher.release)
	if err := mgr.waitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err := s.QueryLatestOpenCodeForAccount(account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatalf("stale in-flight result was persisted: %+v", latest)
	}
}

func TestOpenCodeAgentManagerCancellationClearsQueuedJobs(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, workspace := range []string{"a", "b", "c", "d"} {
		if _, err := s.CreateOpenCodeAccount(workspace, workspace, "cookie", true); err != nil {
			t.Fatal(err)
		}
	}
	fetcher := &blockingOpenCodeFetcher{started: make(chan struct{}, 4), release: make(chan struct{})}
	mgr := NewOpenCodeAgentManager(fetcher, s, nil, 120*time.Second, slog.Default())
	mgr.SetWorkerCount(1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.pollDueAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := mgr.waitIdle(waitCtx); err != nil {
		t.Fatalf("queued jobs were not released after cancellation: %v", err)
	}
	mgr.pendingMu.Lock()
	pending := len(mgr.pending)
	mgr.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending accounts after cancellation = %d", pending)
	}
}

func TestOpenCodeAgentManagerRunWaitsForWorkersOnShutdown(t *testing.T) {
	t.Setenv("ONWATCH_CREDENTIAL_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateOpenCodeAccount("A", "ws-a", "cookie", true); err != nil {
		t.Fatal(err)
	}
	fetcher := &blockingOpenCodeFetcher{
		started: make(chan struct{}, 1), release: make(chan struct{}),
		cancelDelay: 75 * time.Millisecond, finished: make(chan struct{}, 1),
	}
	mgr := NewOpenCodeAgentManager(fetcher, s, nil, 120*time.Second, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mgr.Run(ctx) }()
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	cancel()
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before worker cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-fetcher.finished:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after cancellation")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish after worker cleanup")
	}
}
