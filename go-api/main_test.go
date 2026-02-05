package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, 0x54,
	0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44,
	0xAE, 0x42, 0x60, 0x82,
}

func resetTestState() {
	cacheMu.Lock()
	displayCaches = make(map[int]*displayCache)
	failLogs = nil
	cacheMu.Unlock()

	tagoMu.Lock()
	tagoCaches = make(map[int]*tagoCache)
	tagoMu.Unlock()
}

func TestServeScreenPNGHeaders(t *testing.T) {
	resetTestState()

	tmpDir := t.TempDir()
	screenPath := filepath.Join(tmpDir, "screen.png")
	if err := os.WriteFile(screenPath, tinyPNG, 0644); err != nil {
		t.Fatalf("write screen png: %v", err)
	}
	t.Setenv("SCREEN_PATH", screenPath)

	req := httptest.NewRequest(http.MethodGet, "/display/screen.png", nil)
	w := httptest.NewRecorder()
	serveScreenPNG(w, req, 1)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if res.Header.Get("Cache-Control") == "" {
		t.Fatalf("Cache-Control header missing")
	}
	if res.Header.Get("ETag") == "" {
		t.Fatalf("ETag header missing")
	}
	if res.Header.Get("Last-Modified") == "" {
		t.Fatalf("Last-Modified header missing")
	}
}

func TestHealthIncludesKeys(t *testing.T) {
	resetTestState()

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cacheMu.Lock()
	displayCaches[1] = &displayCache{
		Raw:           []byte(`{}`),
		LastGoodAt:    now,
		LastOkStateAt: now,
		LastOkPNGAt:   now,
	}
	cacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode health json: %v", err)
	}
	if _, ok := payload["recent_fails"]; !ok {
		t.Fatalf("missing recent_fails")
	}
	if _, ok := payload["last_ok_state_at"]; !ok {
		t.Fatalf("missing last_ok_state_at")
	}
	if _, ok := payload["last_ok_png_at"]; !ok {
		t.Fatalf("missing last_ok_png_at")
	}

	displays, ok := payload["displays"].(map[string]any)
	if !ok {
		t.Fatalf("displays not an object")
	}
	display1, ok := displays["1"].(map[string]any)
	if !ok {
		t.Fatalf("missing display 1")
	}
	if _, ok := display1["last_ok_state_at"]; !ok {
		t.Fatalf("display 1 missing last_ok_state_at")
	}
	if _, ok := display1["last_ok_png_at"]; !ok {
		t.Fatalf("display 1 missing last_ok_png_at")
	}
}
