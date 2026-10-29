package management

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clauderesets"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func bankedOAuth(a *coreauth.Auth) bool {
	if a == nil || a.AuthKind() != coreauth.AuthKindOAuth {
		return false
	}
	token, ok := a.Metadata["access_token"].(string)
	return a.ID != "" && a.Provider == "claude" && !a.Disabled && a.Status != coreauth.StatusDisabled && a.Attributes["api_key"] == "" && ok && token != ""
}

// StartBankedResets starts one conservative worker for the HTTP server lifetime.
func (h *Handler) StartBankedResets() {
	h.resetsLifeMu.Lock()
	defer h.resetsLifeMu.Unlock()
	if h.resetsCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.resetsCancel = cancel
	h.resetsDone = make(chan struct{})
	go func() {
		defer close(h.resetsDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.bankedResetTick(ctx)
			}
		}
	}()
}
func (h *Handler) StopBankedResets() {
	h.resetsLifeMu.Lock()
	defer h.resetsLifeMu.Unlock()
	if h.resetsCancel == nil {
		return
	}
	h.resetsCancel()
	<-h.resetsDone
	h.resetsCancel = nil
}
func (h *Handler) bankedResetTick(ctx context.Context) {
	h.mu.Lock()
	cfg, manager := h.cfg, h.authManager
	ctx, cancel := context.WithCancel(ctx)
	h.resetsPreflightCancel = cancel
	h.mu.Unlock()
	defer func() { cancel(); h.mu.Lock(); h.resetsPreflightCancel = nil; h.mu.Unlock() }()
	if cfg == nil || !cfg.Claude.AutoApplyBankedResets || manager == nil {
		return
	}
	for _, a := range manager.List() {
		if !bankedOAuth(a) {
			continue
		}
		valid := func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			current, ok := manager.GetByID(a.ID)
			return ok && h.authManager == manager && h.cfg == cfg && cfg.Claude.AutoApplyBankedResets && sameBankedSession(a, current)
		}
		call := func(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
			// Linearize dispatch with management config changes. Preflight never holds this lock.
			if method == http.MethodPost {
				h.mu.Lock()
				defer h.mu.Unlock()
				current, ok := manager.GetByID(a.ID)
				if !ok || h.cfg != cfg || h.authManager != manager || !cfg.Claude.AutoApplyBankedResets || !sameBankedSession(a, current) || ctx.Err() != nil {
					return 0, nil, clauderesets.ErrPaused
				}
			} else if !valid() {
				return 0, nil, clauderesets.ErrPaused
			}
			req, err := http.NewRequestWithContext(ctx, method, "https://api.anthropic.com"+path, bytes.NewReader(body))
			if err != nil {
				return 0, nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
			req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
			// Reuse management quota credential resolution and account/global proxy selection.
			snapshot := &Handler{cfg: cfg}
			token, err := snapshot.resolveTokenForAuth(ctx, a, "")
			if err != nil || token == "" {
				return 0, nil, clauderesets.ErrPaused
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.GetBody = nil // A spending POST must never be replayed by transport or redirect handling.
			client := &http.Client{Transport: snapshot.apiCallTransport(a, ""), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			var resp *http.Response
			if method == http.MethodPost {
				err = manager.WithAccountOperation(ctx, a.ID, func(current *coreauth.Auth) error {
					if !sameBankedSession(a, current) {
						return clauderesets.ErrPaused
					}
					var errDo error
					resp, errDo = client.Do(req)
					return errDo
				})
			} else {
				resp, err = client.Do(req)
			}
			if err != nil {
				return 0, nil, err
			}
			defer func() { _ = resp.Body.Close() }()
			if method == http.MethodGet && resp.StatusCode == http.StatusUnauthorized && valid() {
				// Use the manager's existing refresh serialization; never replay a spend.
				_, _ = manager.ForceRefreshAuth(ctx, a.ID)
				return 0, nil, clauderesets.ErrPaused
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
			if err != nil || len(b) > 1024*1024 {
				return 0, nil, clauderesets.ErrMalformed
			}
			return resp.StatusCode, b, nil
		}
		h.resets.RunAccount(ctx, a.ID, valid, call)
	}
}
func sameBankedSession(a, b *coreauth.Auth) bool {
	return bankedOAuth(b) && a.RegistrationEpoch == b.RegistrationEpoch && a.CredentialVersion == b.CredentialVersion && a.Generation == b.Generation && a.ProxyURL == b.ProxyURL && reflect.DeepEqual(a.Metadata, b.Metadata) && reflect.DeepEqual(a.Attributes, b.Attributes)
}
