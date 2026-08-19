package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/onllm-dev/onwatch/v2/internal/api"
)

const providerCredentialTestTimeout = 12 * time.Second

func providerCredentialError(err error) string {
	switch {
	case errors.Is(err, api.ErrUnauthorized),
		errors.Is(err, api.ErrZaiUnauthorized),
		errors.Is(err, api.ErrCopilotUnauthorized),
		errors.Is(err, api.ErrCopilotForbidden),
		errors.Is(err, api.ErrOpenRouterUnauthorized),
		errors.Is(err, api.ErrMoonshotUnauthorized),
		errors.Is(err, api.ErrDeepSeekUnauthorized):
		return "authentication_failed"
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, api.ErrNetworkError),
		errors.Is(err, api.ErrZaiNetworkError),
		errors.Is(err, api.ErrCopilotNetworkError),
		errors.Is(err, api.ErrOpenRouterNetworkError),
		errors.Is(err, api.ErrMoonshotNetworkError),
		errors.Is(err, api.ErrDeepSeekNetworkError):
		return "network_error"
	case errors.Is(err, api.ErrInvalidResponse),
		errors.Is(err, api.ErrZaiInvalidResponse),
		errors.Is(err, api.ErrCopilotInvalidResponse),
		errors.Is(err, api.ErrOpenRouterInvalidResponse),
		errors.Is(err, api.ErrMoonshotInvalidResponse),
		errors.Is(err, api.ErrDeepSeekInvalidResponse):
		return "quota_parse_failed"
	default:
		return "connection_failed"
	}
}

func normalizeProviderCredentialValues(provider string, values map[string]string) (map[string]string, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	normalized := map[string]string{}
	secretField := "api_key"
	if provider == "copilot" {
		secretField = "token"
	}
	switch provider {
	case "synthetic", "zai", "copilot", "openrouter", "moonshot", "deepseek":
	default:
		return nil, false
	}
	secret := strings.TrimSpace(values[secretField])
	if secret == "" || len(secret) > 16384 {
		return nil, false
	}
	normalized[secretField] = secret
	if provider == "zai" {
		region := strings.ToLower(strings.TrimSpace(values["region"]))
		if region == "" {
			region = "global"
		}
		if region != "global" && region != "cn" {
			return nil, false
		}
		normalized["region"] = region
	}
	return normalized, true
}

func (h *Handler) testProviderCredentialConnection(ctx context.Context, provider string, values map[string]string) error {
	// Connection tests intentionally discard client logs. Even redacted values
	// should not expose any portion of a credential entered in the browser.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	switch provider {
	case "synthetic":
		_, err := api.NewClient(values["api_key"], logger, api.WithTimeout(providerCredentialTestTimeout)).FetchQuotas(ctx)
		return err
	case "zai":
		opts := []api.ZaiOption{api.WithZaiTimeout(providerCredentialTestTimeout)}
		if values["region"] == "cn" {
			opts = append(opts, api.WithZaiBaseURL("https://open.bigmodel.cn/api/monitor/usage/quota/limit"))
		}
		_, err := api.NewZaiClient(values["api_key"], logger, opts...).FetchQuotas(ctx)
		return err
	case "copilot":
		_, err := api.NewCopilotClient(values["token"], logger, api.WithCopilotTimeout(providerCredentialTestTimeout)).FetchQuotas(ctx)
		return err
	case "openrouter":
		_, err := api.NewOpenRouterClient(values["api_key"], logger, api.WithOpenRouterTimeout(providerCredentialTestTimeout)).FetchUsage(ctx)
		return err
	case "moonshot":
		_, err := api.NewMoonshotClient(values["api_key"], logger, api.WithMoonshotTimeout(providerCredentialTestTimeout)).FetchBalance(ctx)
		return err
	case "deepseek":
		_, err := api.NewDeepSeekClient(values["api_key"], logger, api.WithDeepSeekTimeout(providerCredentialTestTimeout)).FetchBalance(ctx)
		return err
	default:
		return errors.New("unsupported provider")
	}
}

// ProviderCredentialTest checks unsaved single-account credentials without
// persisting or echoing them.
func (h *Handler) ProviderCredentialTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Provider string            `json:"provider"`
		Settings map[string]string `json:"settings"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := decodeStrictJSON(r.Body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_credentials")
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	values, ok := normalizeProviderCredentialValues(provider, req.Settings)
	if !ok {
		respondError(w, http.StatusBadRequest, "invalid_credentials")
		return
	}
	test := h.providerCredentialConnectionTest
	if test == nil {
		test = h.testProviderCredentialConnection
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerCredentialTestTimeout)
	defer cancel()
	if err := test(ctx, provider, values); err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"success": false, "error": providerCredentialError(err)})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}
