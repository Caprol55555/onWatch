package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/notify"
	"github.com/onllm-dev/onwatch/v2/internal/store"
	"github.com/onllm-dev/onwatch/v2/internal/tracker"
)

const (
	openCodeDefaultWorkers = 2
	openCodeMaxWorkers     = 3
	openCodePollTimeout    = 10 * time.Second
	openCodeMaxBackoff     = 15 * time.Minute
	openCodeQueueSize      = 64
)

type openCodePollJob struct {
	account store.OpenCodeAccount
}

type OpenCodeAgentManager struct {
	client         openCodeFetcher
	store          *store.Store
	tracker        *tracker.OpenCodeTracker
	interval       time.Duration
	logger         *slog.Logger
	notifier       *notify.NotificationEngine
	pollingCheck   func() bool
	workerCount    int
	jobs           chan openCodePollJob
	workerMu       sync.Mutex
	workersRunning bool
	workerCtx      context.Context
	workersDone    chan struct{}
	pendingMu      sync.Mutex
	pending        map[int64]bool
	wg             sync.WaitGroup
	sessionsMu     sync.Mutex
	sessions       map[int64]*SessionManager
}

func NewOpenCodeAgentManager(client openCodeFetcher, st *store.Store, tr *tracker.OpenCodeTracker, interval time.Duration, logger *slog.Logger) *OpenCodeAgentManager {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 120 * time.Second
	}
	return &OpenCodeAgentManager{client: client, store: st, tracker: tr, interval: interval, logger: logger, workerCount: openCodeDefaultWorkers, jobs: make(chan openCodePollJob, openCodeQueueSize), pending: make(map[int64]bool), sessions: make(map[int64]*SessionManager)}
}

func (m *OpenCodeAgentManager) SetWorkerCount(count int) {
	if count < 1 {
		count = 1
	}
	if count > openCodeMaxWorkers {
		count = openCodeMaxWorkers
	}
	m.workerCount = count
}

func (m *OpenCodeAgentManager) SetNotifier(n *notify.NotificationEngine) { m.notifier = n }
func (m *OpenCodeAgentManager) SetPollingCheck(fn func() bool)           { m.pollingCheck = fn }

func (m *OpenCodeAgentManager) Run(ctx context.Context) error {
	if m.client == nil || m.store == nil {
		return nil
	}
	m.startWorkers(ctx)
	m.logger.Info("OpenCode multi-account agent started", "interval", m.interval, "workers", m.workerCount)
	defer func() {
		if m.waitForWorkers(openCodePollTimeout + time.Second) {
			m.sessionsMu.Lock()
			for _, sm := range m.sessions {
				sm.Close()
			}
			m.sessionsMu.Unlock()
		} else {
			m.logger.Error("Timed out waiting for OpenCode workers to stop")
		}
		m.logger.Info("OpenCode multi-account agent stopped")
	}()
	if err := m.pollDueAccounts(ctx); err != nil {
		m.logger.Error("Failed to schedule OpenCode accounts", "error", err)
	}
	scanInterval := m.interval / 4
	if scanInterval < time.Second {
		scanInterval = time.Second
	}
	if scanInterval > 30*time.Second {
		scanInterval = 30 * time.Second
	}
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := m.pollDueAccounts(ctx); err != nil {
				m.logger.Error("Failed to schedule OpenCode accounts", "error", err)
			}
		}
	}
}

func (m *OpenCodeAgentManager) startWorkers(ctx context.Context) {
	for {
		m.workerMu.Lock()
		if !m.workersRunning {
			m.workersRunning = true
			m.workerCtx = ctx
			m.workersDone = make(chan struct{})
			break
		}
		if m.workerCtx == nil || m.workerCtx.Err() == nil {
			m.workerMu.Unlock()
			return
		}
		done := m.workersDone
		m.workerMu.Unlock()
		select {
		case <-done:
			continue
		case <-ctx.Done():
			return
		}
	}
	done := m.workersDone
	m.workerMu.Unlock()

	var workers sync.WaitGroup
	for i := 0; i < m.workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-m.jobs:
					m.pollAccount(ctx, job.account)
					m.finishJob(job)
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		m.workerMu.Lock()
		for {
			select {
			case job := <-m.jobs:
				m.finishJob(job)
			default:
				m.workersRunning = false
				m.workerCtx = nil
				close(done)
				m.workerMu.Unlock()
				return
			}
		}
	}()
}

func (m *OpenCodeAgentManager) finishJob(job openCodePollJob) {
	m.pendingMu.Lock()
	delete(m.pending, job.account.AccountID)
	m.pendingMu.Unlock()
	m.wg.Done()
}

func (m *OpenCodeAgentManager) workersActive() bool {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	return m.workersRunning
}

func (m *OpenCodeAgentManager) waitForWorkers(timeout time.Duration) bool {
	m.workerMu.Lock()
	if !m.workersRunning || m.workersDone == nil {
		m.workerMu.Unlock()
		return true
	}
	done := m.workersDone
	m.workerMu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (m *OpenCodeAgentManager) enqueueJob(ctx context.Context, job openCodePollJob) (queued, full bool) {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if !m.workersRunning || m.workerCtx != ctx || ctx.Err() != nil {
		return false, false
	}
	select {
	case m.jobs <- job:
		return true, false
	default:
		return false, true
	}
}

func (m *OpenCodeAgentManager) pollDueAccounts(ctx context.Context) error {
	if m.client == nil || m.store == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	if m.pollingCheck != nil && !m.pollingCheck() {
		return nil
	}
	m.startWorkers(ctx)
	if !m.workersActive() {
		return nil
	}
	accounts, err := m.store.QueryOpenCodeAccounts(false)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, public := range accounts {
		if !public.Enabled || public.AuthStatus == store.OpenCodeAuthNeedsReauth || public.AuthStatus == store.OpenCodeAuthUnauthorized {
			continue
		}
		if public.NextPollAt != nil && public.NextPollAt.After(now) {
			continue
		}
		account, err := m.store.GetOpenCodeAccount(public.AccountID, true)
		if err != nil {
			m.logger.Error("Failed to load OpenCode account credential", "account_id", public.AccountID, "error", err)
			_, _ = m.store.UpdateOpenCodeAccountPollState(public.AccountID, public.CredentialVersion, store.OpenCodeAuthError, "credential_decrypt", false, now.Add(m.interval))
			continue
		}
		if account == nil || account.AuthCookie == "" {
			continue
		}
		m.pendingMu.Lock()
		if m.pending[account.AccountID] {
			m.pendingMu.Unlock()
			continue
		}
		m.pending[account.AccountID] = true
		m.wg.Add(1)
		m.pendingMu.Unlock()
		queued, full := m.enqueueJob(ctx, openCodePollJob{account: *account})
		if !queued {
			m.pendingMu.Lock()
			delete(m.pending, account.AccountID)
			m.pendingMu.Unlock()
			m.wg.Done()
			if full {
				m.logger.Warn("OpenCode poll queue full", "account_id", account.AccountID)
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
	return nil
}

func (m *OpenCodeAgentManager) waitIdle(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *OpenCodeAgentManager) pollAccount(parent context.Context, account store.OpenCodeAccount) {
	ctx, cancel := context.WithTimeout(parent, openCodePollTimeout)
	defer cancel()
	snapshot, err := m.client.FetchSnapshot(ctx, account.WorkspaceID, account.AuthCookie)
	if err != nil {
		if parent.Err() != nil {
			return
		}
		m.handlePollError(account, err)
		return
	}
	current, err := m.store.GetOpenCodeAccount(account.AccountID, false)
	if err != nil || current == nil || current.CredentialVersion != account.CredentialVersion || !current.Enabled {
		return
	}
	if _, err := m.store.InsertOpenCodeSnapshotForAccount(account.AccountID, snapshot); err != nil {
		m.logger.Error("Failed to insert OpenCode snapshot", "account_id", account.AccountID, "error", err)
		m.handlePollError(account, err)
		return
	}
	if m.tracker != nil {
		if err := m.tracker.ProcessForAccount(account.AccountID, snapshot); err != nil {
			m.logger.Error("OpenCode tracker processing failed", "account_id", account.AccountID, "error", err)
		}
	}
	next := time.Now().UTC().Add(m.jitter(m.interval))
	if _, err := m.store.UpdateOpenCodeAccountPollState(account.AccountID, account.CredentialVersion, store.OpenCodeAuthValid, "", true, next); err != nil {
		m.logger.Error("Failed to update OpenCode poll state", "account_id", account.AccountID, "error", err)
	}
	if m.notifier != nil {
		for _, q := range snapshot.Quotas {
			m.notifier.Check(openCodeNotificationStatus(account.AccountID, q))
		}
	}
	m.sessionsMu.Lock()
	sm := m.sessions[account.AccountID]
	if sm == nil {
		sm = NewSessionManager(m.store, fmt.Sprintf("opencode:%d", account.AccountID), 5*time.Minute, m.logger)
		m.sessions[account.AccountID] = sm
	}
	m.sessionsMu.Unlock()
	values := make([]float64, 0, len(snapshot.Quotas))
	for _, q := range snapshot.Quotas {
		values = append(values, q.Utilization)
	}
	sm.ReportPoll(values)
	m.logger.Info("OpenCode poll complete", "account_id", account.AccountID, "plan_name", snapshot.PlanName, "quota_count", len(snapshot.Quotas))
}

func openCodeNotificationStatus(accountID int64, quota api.OpenCodeQuota) notify.QuotaStatus {
	return notify.QuotaStatus{
		Provider:    "opencode",
		QuotaKey:    quota.Name,
		AccountID:   strconv.FormatInt(accountID, 10),
		Utilization: quota.Utilization,
		Limit:       quota.Limit,
		ResetsAt:    quota.ResetsAt,
	}
}

func (m *OpenCodeAgentManager) handlePollError(account store.OpenCodeAccount, err error) {
	status, code, terminal := store.OpenCodeAuthError, "fetch_error", false
	switch {
	case errors.Is(err, api.ErrOpenCodeUnauthorized):
		code = "unauthorized"
		if account.ConsecutiveFailures >= 1 && account.LastErrorCode == code {
			status, terminal = store.OpenCodeAuthNeedsReauth, true
		}
	case errors.Is(err, api.ErrOpenCodeForbidden):
		code = "forbidden"
		if account.ConsecutiveFailures >= 1 && account.LastErrorCode == code {
			status, terminal = store.OpenCodeAuthUnauthorized, true
		}
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case errors.Is(err, api.ErrOpenCodeNetworkError):
		code = "network"
	case errors.Is(err, api.ErrOpenCodeServerError):
		code = "server"
	case errors.Is(err, api.ErrOpenCodeParseFailed):
		code = "parse"
	}
	next := time.Now().UTC().Add(m.backoff(account.ConsecutiveFailures + 1))
	if terminal {
		next = time.Time{}
	}
	applied, updateErr := m.store.UpdateOpenCodeAccountPollState(account.AccountID, account.CredentialVersion, status, code, false, next)
	if updateErr != nil {
		m.logger.Error("Failed to update OpenCode error state", "account_id", account.AccountID, "error", updateErr)
		return
	}
	if !applied {
		return
	}
	m.logger.Warn("OpenCode poll failed", "account_id", account.AccountID, "error_code", code)
	if terminal && m.notifier != nil && account.AuthStatus != status {
		m.notifier.SendAuthErrorNotification(notify.AuthErrorAlert{Provider: "opencode", AccountID: strconv.FormatInt(account.AccountID, 10), Title: "OpenCode Go 认证已失效", Message: fmt.Sprintf("账号 %s 需要更新认证 Cookie。", account.Name), IsRecovable: false})
	}
}

func (m *OpenCodeAgentManager) jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Second
	}
	spread := int64(base / 10)
	if spread < 1 {
		return base
	}
	return base + time.Duration(rand.Int64N(spread*2+1)-spread)
}

func (m *OpenCodeAgentManager) backoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := m.interval
	for i := 1; i < failures && d < openCodeMaxBackoff; i++ {
		d *= 2
	}
	if d > openCodeMaxBackoff {
		d = openCodeMaxBackoff
	}
	return m.jitter(d)
}
