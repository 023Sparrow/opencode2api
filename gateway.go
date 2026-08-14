package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxRequestBody = 32 << 20

type Gateway struct {
	cfg      Config
	logger   *slog.Logger
	zenNodes *nodePool
	goNodes  *nodePool
	catalog  *modelCatalog
}

func NewGateway(cfg Config, logger *slog.Logger) (*Gateway, error) {
	transports, err := newTransportPool(cfg.Proxies, cfg.Performance, time.Duration(cfg.Retry.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := newNodePool(cfg.ZenKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("zen node pool: %w", err)
	}
	goNodes, err := newNodePool(cfg.GoKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("go node pool: %w", err)
	}
	return &Gateway{
		cfg:      cfg,
		logger:   logger,
		zenNodes: zenNodes,
		goNodes:  goNodes,
		catalog:  newModelCatalog(cfg.Models.Protocols),
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.handleInference(ProtocolChat)))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.handleInference(ProtocolResponses)))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.handleInference(ProtocolAnthropic)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	return recoveryMiddleware(g.logger, mux)
}

func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{strings.TrimSpace(r.Header.Get("x-api-key"))}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(auth[7:]))
		}
		valid := false
		for _, key := range g.cfg.ServerKeys {
			for _, candidate := range candidates {
				if len(candidate) == len(key) && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
					valid = true
				}
			}
		}
		if !valid {
			protocol := ProtocolChat
			if r.URL.Path == "/v1/messages" {
				protocol = ProtocolAnthropic
			}
			writeAPIError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		next(w, r)
	}
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	models := g.catalog.List()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !supportedModel(model) {
			continue
		}
		data = append(data, map[string]any{"id": model, "object": "model", "created": now, "owned_by": "opencode"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleInference(external Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := stringAt(payload, "model")
		if model == "" {
			writeAPIError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if !supportedModel(model) {
			writeAPIError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			return
		}
		route, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0)
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		encoded := body
		if external != route.Protocol {
			upstreamPayload, err := convertRequest(external, route.Protocol, payload)
			if err != nil {
				writeAPIError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
				return
			}
			encoded, err = json.Marshal(upstreamPayload)
			if err != nil {
				writeAPIError(w, external, http.StatusBadRequest, "request contains unsupported JSON values", "invalid_request_error", "")
				return
			}
		}
		ids := deriveRequestIDs(r, payload)
		stream := boolAt(payload, "stream")
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		resp, err := g.doUpstream(requestCtx, route, encoded, ids)
		if err != nil {
			g.logger.Warn("upstream request failed", "request_id", ids.Request, "tier", route.Tier, "error", err)
			writeAPIError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			if external == route.Protocol {
				_, err = io.Copy(w, resp.Body)
			} else {
				err = transcodeStream(w, resp.Body, route.Protocol, external, model)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Debug("stream ended with error", "request_id", ids.Request, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			writeAPIError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		if external != route.Protocol {
			responseBody, err = convertResponse(route.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response conversion failed", "request_id", ids.Request, "error", err)
				writeAPIError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

func (g *Gateway) doUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs) (*http.Response, error) {
	var lastResponse *http.Response
	var lastErr error
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if route.Tier == TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	cursor := nodes.Cursor()
	if nodes.Len() == 0 {
		return nil, fmt.Errorf("no %s nodes configured", route.Tier)
	}
	for attempt := 1; attempt <= g.cfg.Retry.MaxAttempts; attempt++ {
		node := cursor.Next()
		if node == nil {
			break
		}
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		endpoint := strings.TrimRight(baseURL, "/") + protocolPath(route.Protocol)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("User-Agent", opencodeUserAgent())
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-session", ids.Session)
		req.Header.Set("x-opencode-request", ids.Request)
		req.Header.Set("x-opencode-project", ids.Project)
		if route.Protocol == ProtocolAnthropic {
			req.Header.Set("x-api-key", node.key)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+node.key)
		}
		resp, err := node.client.Do(req)
		if err == nil && resp.StatusCode/100 == 2 {
			nodes.MarkSuccess(node)
			g.logger.Debug("upstream accepted request", "request_id", ids.Request, "attempt", attempt, "tier", route.Tier, "proxy", redactURL(node.proxy))
			return resp, nil
		}
		nodes.MarkFailure(node, resp, err)
		lastResponse = resp
		lastErr = err
		if err != nil {
			g.logger.Debug("upstream transport error", "request_id", ids.Request, "attempt", attempt, "proxy", redactURL(node.proxy), "error", err)
		} else {
			g.logger.Debug("upstream rejected request", "request_id", ids.Request, "attempt", attempt, "status", resp.StatusCode, "proxy", redactURL(node.proxy))
		}
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	return nil, lastErr
}

func protocolPath(protocol Protocol) string {
	switch protocol {
	case ProtocolResponses:
		return "/v1/responses"
	case ProtocolAnthropic:
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

func (g *Gateway) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		var zen, goModels []string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); zen = g.refreshTier(ctx, g.cfg.Upstream.Zen, g.zenNodes) }()
		go func() { defer wg.Done(); goModels = g.refreshTier(ctx, g.cfg.Upstream.Go, g.goNodes) }()
		wg.Wait()
		if zen != nil || goModels != nil {
			g.catalog.Replace(zen, goModels)
			g.logger.Info("model catalog refreshed", "models", len(g.catalog.List()))
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(time.Duration(g.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (g *Gateway) refreshTier(ctx context.Context, base string, nodes *nodePool) []string {
	cursor := nodes.Cursor()
	for attempt := 0; attempt < g.cfg.Retry.MaxAttempts; attempt++ {
		node := cursor.Next()
		if node == nil {
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, err := fetchModels(refreshCtx, node.client, base, node.key)
		cancel()
		if err == nil {
			nodes.MarkSuccess(node)
			return models
		}
		nodes.MarkFailure(node, nil, err)
		g.logger.Debug("model refresh attempt failed", "upstream", base, "attempt", attempt+1, "error", err)
	}
	g.logger.Warn("model refresh failed", "upstream", base)
	return nil
}

func copyErrorResponse(w http.ResponseWriter, protocol Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = firstString(stringAt(value, "error", "message"), stringAt(value, "message"), message)
	}
	writeAPIError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}

func writeAPIError(w http.ResponseWriter, protocol Protocol, status int, message, kind, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("x-request-id", requestID)
	}
	if protocol == ProtocolAnthropic {
		writeJSONStatus(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
		return
	}
	writeJSONStatus(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": nil}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	writeJSONStatus(w, status, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request panic", "error", value)
				writeAPIError(w, ProtocolChat, http.StatusInternalServerError, "internal server error", "server_error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
