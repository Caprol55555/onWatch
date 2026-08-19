package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
	"github.com/onllm-dev/onwatch/v2/internal/store"
)

const (
	providerAccountImportLimit = 1 << 20
	providerAccountImportRows  = 100
)

type accountImportResult struct {
	Row     int    `json:"row"`
	Name    string `json:"name"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func writeAccountTemplate(w http.ResponseWriter, filename, header, example string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "\ufeff"+header+"\n"+example+"\n")
}

// OpenCodeAccountsTemplate downloads the CSV import template.
func (h *Handler) OpenCodeAccountsTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeAccountTemplate(w, "opencode-accounts-template.csv", "name,workspace_id,auth_cookie,enabled", "示例账号,workspace-id,auth-cookie,true")
}

// MiniMaxAccountsTemplate downloads the CSV import template.
func (h *Handler) MiniMaxAccountsTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeAccountTemplate(w, "minimax-accounts-template.csv", "name,api_key,region,enabled", "example-account,api-key,global,true")
}

func readAccountCSV(w http.ResponseWriter, r *http.Request, expected []string) ([][]string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, providerAccountImportLimit)
	reader := csv.NewReader(r.Body)
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = len(expected)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("invalid_csv")
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("missing_header")
	}
	if len(records)-1 > providerAccountImportRows {
		return nil, fmt.Errorf("too_many_rows")
	}
	for i, want := range expected {
		got := strings.TrimSpace(strings.TrimPrefix(records[0][i], "\ufeff"))
		if !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("invalid_header")
		}
	}
	return records[1:], nil
}

func parseCSVEnabled(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid_enabled")
	}
	return enabled, nil
}

// OpenCodeAccountsImport imports up to 100 OpenCode accounts without echoing credentials.
func (h *Handler) OpenCodeAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	records, err := readAccountCSV(w, r, []string{"name", "workspace_id", "auth_cookie", "enabled"})
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	results := make([]accountImportResult, 0, len(records))
	imported := 0
	for i, record := range records {
		name, workspaceID, cookie := strings.TrimSpace(record[0]), strings.TrimSpace(record[1]), strings.TrimSpace(record[2])
		result := accountImportResult{Row: i + 2, Name: name}
		enabled, parseErr := parseCSVEnabled(record[3])
		if name == "" || len(name) > 80 || workspaceID == "" || len(workspaceID) > 256 || cookie == "" || len(cookie) > 16384 {
			result.Error = "invalid_fields"
		} else if parseErr != nil {
			result.Error = parseErr.Error()
		} else if _, createErr := h.store.CreateOpenCodeAccount(name, workspaceID, cookie, enabled); createErr != nil {
			if errors.Is(createErr, store.ErrOpenCodeWorkspaceExists) {
				result.Error = "workspace_exists"
			} else {
				result.Error = "create_failed"
			}
		} else {
			result.Success = true
			imported++
		}
		results = append(results, result)
	}
	if imported > 0 {
		if h.config != nil {
			h.config.OpenCodeAccountsConfigured = true
		}
		if h.agentManager != nil && h.providerPollingEnabled("opencode", h.providerVisibilityMap()) {
			_ = h.agentManager.Start("opencode")
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"imported": imported, "failed": len(records) - imported, "results": results})
}

// MiniMaxAccountsImport imports up to 100 encrypted MiniMax accounts.
func (h *Handler) MiniMaxAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.store == nil {
		respondError(w, http.StatusServiceUnavailable, "storage unavailable")
		return
	}
	records, err := readAccountCSV(w, r, []string{"name", "api_key", "region", "enabled"})
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	results := make([]accountImportResult, 0, len(records))
	imported := 0
	for i, record := range records {
		name, apiKey, region := strings.TrimSpace(record[0]), strings.TrimSpace(record[1]), strings.ToLower(strings.TrimSpace(record[2]))
		result := accountImportResult{Row: i + 2, Name: name}
		enabled, parseErr := parseCSVEnabled(record[3])
		if name == "" || !validProfileName.MatchString(name) || apiKey == "" {
			result.Error = "invalid_fields"
		} else if region != "global" && region != "cn" {
			result.Error = "invalid_region"
		} else if parseErr != nil {
			result.Error = parseErr.Error()
		} else {
			accounts, queryErr := h.store.QueryProviderAccounts("minimax")
			if queryErr != nil {
				result.Error = "query_failed"
				results = append(results, result)
				continue
			}
			duplicate := false
			for _, account := range accounts {
				if account.DeletedAt == nil && account.Name == name {
					duplicate = true
					break
				}
			}
			if duplicate {
				result.Error = "account_exists"
				results = append(results, result)
				continue
			}
			account, createErr := h.store.CreateOrRestoreProviderAccount("minimax", name)
			if createErr != nil {
				result.Error = "create_failed"
			} else {
				ciphertext, encryptErr := h.store.EncryptProviderSecret(fmt.Sprintf("minimax:%d", account.ID), "api_key", apiKey)
				if encryptErr != nil {
					_ = h.store.MarkProviderAccountDeletedByID(account.ID)
					result.Error = "encrypt_failed"
				} else {
					metadata := fmt.Sprintf(`{"api_key":%q,"region":%q}`, ciphertext, region)
					if updateErr := h.store.UpdateProviderAccountMetadata(account.ID, metadata); updateErr != nil {
						_ = h.store.MarkProviderAccountDeletedByID(account.ID)
						result.Error = "save_failed"
					} else if stateErr := h.setProviderVisibility(fmt.Sprintf("minimax:%d", account.ID), &enabled, nil); stateErr != nil {
						_ = h.store.MarkProviderAccountDeletedByID(account.ID)
						result.Error = "save_failed"
					} else {
						result.Success = true
						imported++
					}
				}
			}
		}
		results = append(results, result)
	}
	if imported > 0 {
		if h.agentManager != nil && h.providerPollingEnabled("minimax", h.providerVisibilityMap()) {
			_ = h.agentManager.Start("minimax")
		}
		if h.minimaxAgentMgr != nil {
			h.minimaxAgentMgr.Reload()
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"imported": imported, "failed": len(records) - imported, "results": results})
}

func openCodeConnectionError(err error) string {
	switch {
	case errors.Is(err, api.ErrOpenCodeUnauthorized), errors.Is(err, api.ErrOpenCodeForbidden):
		return "authentication_failed"
	case errors.Is(err, api.ErrOpenCodeParseFailed):
		return "quota_parse_failed"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, api.ErrOpenCodeNetworkError):
		return "network_error"
	default:
		return "connection_failed"
	}
}

func miniMaxConnectionError(err error) string {
	switch {
	case errors.Is(err, api.ErrMiniMaxUnauthorized):
		return "authentication_failed"
	case errors.Is(err, api.ErrMiniMaxAccessBlocked):
		return "access_blocked"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, api.ErrMiniMaxNetworkError):
		return "network_error"
	default:
		return "connection_failed"
	}
}

// OpenCodeAccountTest checks unsaved credentials and never persists them.
func (h *Handler) OpenCodeAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		AuthCookie  string `json:"auth_cookie"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, openCodeAccountRequestLimit)
	if err := decodeStrictJSON(r.Body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_credentials")
		return
	}
	workspaceID, authCookie := strings.TrimSpace(req.WorkspaceID), strings.TrimSpace(req.AuthCookie)
	if workspaceID == "" || len(workspaceID) > 256 || authCookie == "" || len(authCookie) > 16384 {
		respondError(w, http.StatusBadRequest, "invalid_credentials")
		return
	}
	test := h.openCodeConnectionTest
	if test == nil {
		test = func(ctx context.Context, workspaceID, cookie string) error {
			_, err := api.NewOpenCodeClient(h.logger, api.WithOpenCodeTimeout(12*time.Second)).FetchSnapshot(ctx, workspaceID, cookie)
			return err
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if err := test(ctx, workspaceID, authCookie); err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"success": false, "error": openCodeConnectionError(err)})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// MiniMaxAccountTest checks an unsaved API key and never persists it.
func (h *Handler) MiniMaxAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
		Region string `json:"region"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, openCodeAccountRequestLimit)
	if err := decodeStrictJSON(r.Body, &req); err != nil || strings.TrimSpace(req.APIKey) == "" {
		respondError(w, http.StatusBadRequest, "invalid_credentials")
		return
	}
	region := strings.ToLower(strings.TrimSpace(req.Region))
	if region == "" {
		region = "global"
	}
	if region != "global" && region != "cn" {
		respondError(w, http.StatusBadRequest, "invalid_region")
		return
	}
	test := h.miniMaxConnectionTest
	if test == nil {
		test = func(ctx context.Context, apiKey, region string) error {
			baseURL := "https://api.minimax.io/v1/api/openplatform/coding_plan/remains"
			if region == "cn" {
				baseURL = "https://www.minimaxi.com/v1/api/openplatform/coding_plan/remains"
			}
			_, err := api.NewMiniMaxClient(apiKey, h.logger, api.WithMiniMaxBaseURL(baseURL), api.WithMiniMaxTimeout(12*time.Second)).FetchRemains(ctx)
			return err
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if err := test(ctx, strings.TrimSpace(req.APIKey), region); err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"success": false, "error": miniMaxConnectionError(err)})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func decodeStrictJSON(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
