package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	tunnelMaxInFlight = 64
	tunnelRequestTTL  = 2 * time.Minute
)

type tunnelRequest struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

type tunnelResponse struct {
	ID      string              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// serveTunnel reads relay requests from ws and dispatches them to h.
// Returns when ws is closed or ctx is cancelled.
func serveTunnel(ctx context.Context, ws *websocket.Conn, h http.Handler) {
	defer ws.Close()

	// Cancel ws on ctx done — unblocks a pending ReadMessage.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-done:
		}
	}()

	var writeMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, tunnelMaxInFlight)

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			slog.Debug("tunnel read ended", "err", err)
			break
		}
		var req tunnelRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			slog.Warn("tunnel: malformed request", "err", err)
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(req tunnelRequest) {
			defer wg.Done()
			defer func() { <-sem }()
			dispatchTunnelRequest(ctx, ws, &writeMu, h, req)
		}(req)
	}
	wg.Wait()
}

func dispatchTunnelRequest(ctx context.Context, ws *websocket.Conn, writeMu *sync.Mutex, h http.Handler, req tunnelRequest) {
	reqCtx, cancel := context.WithTimeout(ctx, tunnelRequestTTL)
	defer cancel()

	httpReq := httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body)).WithContext(reqCtx)
	for k, vals := range req.Headers {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httpReq)

	result := rec.Result()
	body, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()

	resp := tunnelResponse{
		ID:      req.ID,
		Status:  result.StatusCode,
		Headers: result.Header,
		Body:    body,
	}
	data, _ := json.Marshal(resp)

	writeMu.Lock()
	err := ws.WriteMessage(websocket.TextMessage, data)
	writeMu.Unlock()
	if err != nil {
		slog.Debug("tunnel write failed", "id", req.ID, "err", err)
	}
}
