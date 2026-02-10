package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestServeScreenPNGPerDisplay(t *testing.T) {
	resetTestState()

	tmpDir := t.TempDir()
	t.Setenv("SCREEN_PATH", "")
	t.Setenv("SCREEN_PATH_PATTERN", filepath.Join(tmpDir, "screen_{id}.png"))

	png1 := append([]byte{}, tinyPNG...)
	png2 := append([]byte{}, tinyPNG...)
	png2[len(png2)-1] = 0x81

	if err := os.WriteFile(filepath.Join(tmpDir, "screen_1.png"), png1, 0644); err != nil {
		t.Fatalf("write display 1 png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "screen_2.png"), png2, 0644); err != nil {
		t.Fatalf("write display 2 png: %v", err)
	}

	assertScreen := func(displayID int, want []byte) {
		req := httptest.NewRequest(http.MethodGet, "/display/"+strconv.Itoa(displayID)+"/screen.png", nil)
		w := httptest.NewRecorder()
		serveScreenPNG(w, req, displayID)

		res := w.Result()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("display %d status = %d, want %d", displayID, res.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("display %d read body: %v", displayID, err)
		}
		if !bytes.Equal(body, want) {
			t.Fatalf("display %d body mismatch", displayID)
		}
	}

	assertScreen(1, png1)
	assertScreen(2, png2)
}

func TestAtomicWriteFileCreatesParentDir(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "2", "screen.png")
	if err := atomicWriteFile(target, tinyPNG); err != nil {
		t.Fatalf("atomicWriteFile failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target file: %v", err)
	}
	if !bytes.Equal(got, tinyPNG) {
		t.Fatalf("written file content mismatch")
	}
}

func TestServeDisplayConfigPerDisplay(t *testing.T) {
	resetTestState()

	raw1 := []byte(`{"display":{"display_id":1}}`)
	raw2 := []byte(`{"display":{"display_id":2}}`)

	cacheMu.Lock()
	displayCaches[1] = &displayCache{Raw: raw1}
	displayCaches[2] = &displayCache{Raw: raw2}
	cacheMu.Unlock()

	req1 := httptest.NewRequest(http.MethodGet, "/v1/display/1/config", nil)
	w1 := httptest.NewRecorder()
	serveDisplayConfig(w1, req1, 1)
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("display 1 config status = %d, want %d", w1.Result().StatusCode, http.StatusOK)
	}
	body1, err := io.ReadAll(w1.Result().Body)
	if err != nil {
		t.Fatalf("read display 1 config body: %v", err)
	}
	if !bytes.Equal(body1, raw1) {
		t.Fatalf("display 1 config mismatch")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/display/2/config", nil)
	w2 := httptest.NewRecorder()
	serveDisplayConfig(w2, req2, 2)
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("display 2 config status = %d, want %d", w2.Result().StatusCode, http.StatusOK)
	}
	body2, err := io.ReadAll(w2.Result().Body)
	if err != nil {
		t.Fatalf("read display 2 config body: %v", err)
	}
	if !bytes.Equal(body2, raw2) {
		t.Fatalf("display 2 config mismatch")
	}
}

func TestServeDisplayStatePerDisplay(t *testing.T) {
	resetTestState()

	cacheMu.Lock()
	displayCaches[1] = &displayCache{
		Raw: []byte(`{}`),
		Config: DisplayConfig{
			Display: Display{DisplayID: 1, Enabled: true, NodeID: "NODE-1"},
			Settings: Setting{
				Theme:      "default",
				RefreshSec: 5,
				MaxRoutes:  5,
			},
			Routes: []struct {
				RouteID   int    `json:"route_id"`
				RouteName string `json:"route_name"`
				Enabled   bool   `json:"enabled"`
				SortOrder int    `json:"sort_order"`
			}{
				{RouteID: 101, RouteName: "101", Enabled: true, SortOrder: 1},
			},
		},
	}
	displayCaches[2] = &displayCache{
		Raw: []byte(`{}`),
		Config: DisplayConfig{
			Display: Display{DisplayID: 2, Enabled: true, NodeID: "NODE-2"},
			Settings: Setting{
				Theme:      "default",
				RefreshSec: 5,
				MaxRoutes:  5,
			},
			Routes: []struct {
				RouteID   int    `json:"route_id"`
				RouteName string `json:"route_name"`
				Enabled   bool   `json:"enabled"`
				SortOrder int    `json:"sort_order"`
			}{
				{RouteID: 202, RouteName: "202", Enabled: true, SortOrder: 1},
			},
		},
	}
	cacheMu.Unlock()

	sec1 := 30
	sec2 := 300
	setTagoCache(1, map[string]ETASnapshot{
		"101": {ETASec: &sec1, Ended: false},
	}, nil)
	setTagoCache(2, map[string]ETASnapshot{
		"202": {ETASec: &sec2, Ended: false},
	}, nil)

	readState := func(displayID int) StateResponse {
		req := httptest.NewRequest(http.MethodGet, "/v1/display/"+strconv.Itoa(displayID)+"/state", nil)
		w := httptest.NewRecorder()
		serveDisplayState(w, req, displayID)
		if w.Result().StatusCode != http.StatusOK {
			t.Fatalf("display %d state status = %d, want %d", displayID, w.Result().StatusCode, http.StatusOK)
		}
		var st StateResponse
		if err := json.NewDecoder(w.Result().Body).Decode(&st); err != nil {
			t.Fatalf("decode display %d state: %v", displayID, err)
		}
		return st
	}

	st1 := readState(1)
	st2 := readState(2)

	if len(st1.Routes) != 1 || len(st2.Routes) != 1 {
		t.Fatalf("unexpected route counts: display1=%d display2=%d", len(st1.Routes), len(st2.Routes))
	}
	if st1.Routes[0].Route == st2.Routes[0].Route {
		t.Fatalf("expected different routes for display 1 and 2")
	}
	if st1.Routes[0].DisplayETA == st2.Routes[0].DisplayETA {
		t.Fatalf("expected different ETA for display 1 and 2")
	}
}

func TestDisplayScreenLegacyPathCompatibility(t *testing.T) {
	resetTestState()

	tmpDir := t.TempDir()
	t.Setenv("SCREEN_PATH", filepath.Join(tmpDir, "legacy.png"))
	t.Setenv("SCREEN_PATH_PATTERN", filepath.Join(tmpDir, "screen_{id}.png"))

	png1 := append([]byte{}, tinyPNG...)
	png2 := append([]byte{}, tinyPNG...)
	png2[len(png2)-1] = 0x81

	if err := os.WriteFile(filepath.Join(tmpDir, "legacy.png"), png1, 0644); err != nil {
		t.Fatalf("write legacy png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "screen_2.png"), png2, 0644); err != nil {
		t.Fatalf("write display 2 png: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/display/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/display/")
		if path == "screen.png" {
			serveScreenPNG(w, r, 1)
			return
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) != 2 || parts[1] != "screen.png" {
			http.NotFound(w, r)
			return
		}
		displayID, err := strconv.Atoi(parts[0])
		if err != nil || displayID <= 0 {
			http.Error(w, "invalid display id", http.StatusBadRequest)
			return
		}
		serveScreenPNG(w, r, displayID)
	})

	reqLegacy := httptest.NewRequest(http.MethodGet, "/display/screen.png", nil)
	wLegacy := httptest.NewRecorder()
	mux.ServeHTTP(wLegacy, reqLegacy)
	if wLegacy.Result().StatusCode != http.StatusOK {
		t.Fatalf("legacy path status = %d, want %d", wLegacy.Result().StatusCode, http.StatusOK)
	}
	bodyLegacy, err := io.ReadAll(wLegacy.Result().Body)
	if err != nil {
		t.Fatalf("read legacy body: %v", err)
	}
	if !bytes.Equal(bodyLegacy, png1) {
		t.Fatalf("legacy path did not serve display 1 image")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/display/2/screen.png", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("display 2 path status = %d, want %d", w2.Result().StatusCode, http.StatusOK)
	}
	body2, err := io.ReadAll(w2.Result().Body)
	if err != nil {
		t.Fatalf("read display 2 body: %v", err)
	}
	if !bytes.Equal(body2, png2) {
		t.Fatalf("display 2 path mismatch")
	}
}

func TestFetchArrivalsTAGORequiresCityCode(t *testing.T) {
	t.Setenv("TAGO_ARVL_SERVICE_KEY", "test-key")
	t.Setenv("TAGO_CITY_CODE", "")

	_, err := fetchArrivalsTAGO("DJB8002304")
	if err == nil {
		t.Fatalf("expected city code error")
	}
	if !strings.Contains(err.Error(), "TAGO_CITY_CODE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetPNGStaleInfo(t *testing.T) {
	now := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	threshold := 3 * time.Minute

	stale, age := getPNGStaleInfo(time.Time{}, now, threshold)
	if !stale || age != -1 {
		t.Fatalf("zero time: stale=%v age=%d, want stale=true age=-1", stale, age)
	}

	stale, age = getPNGStaleInfo(now.Add(-2*time.Minute), now, threshold)
	if stale || age != 120 {
		t.Fatalf("fresh png: stale=%v age=%d, want stale=false age=120", stale, age)
	}

	stale, age = getPNGStaleInfo(now.Add(-4*time.Minute), now, threshold)
	if !stale || age != 240 {
		t.Fatalf("stale png: stale=%v age=%d, want stale=true age=240", stale, age)
	}
}

func TestAddFailLogKeepsRecent50(t *testing.T) {
	resetTestState()

	for i := 1; i <= 55; i++ {
		addFailLog(i, "unit_test", io.EOF)
	}

	cacheMu.RLock()
	defer cacheMu.RUnlock()
	if len(failLogs) != 50 {
		t.Fatalf("failLogs len=%d, want 50", len(failLogs))
	}
	if failLogs[0].DisplayID != 6 {
		t.Fatalf("first log display_id=%d, want 6", failLogs[0].DisplayID)
	}
	if failLogs[len(failLogs)-1].DisplayID != 55 {
		t.Fatalf("last log display_id=%d, want 55", failLogs[len(failLogs)-1].DisplayID)
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
		LastStateHash: "abc123",
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
	if _, ok := display1["tago_last_error"]; !ok {
		t.Fatalf("display 1 missing tago_last_error")
	}
	if _, ok := display1["tago_cache_at"]; !ok {
		t.Fatalf("display 1 missing tago_cache_at")
	}
	if _, ok := display1["png_stale"]; !ok {
		t.Fatalf("display 1 missing png_stale")
	}
	if _, ok := display1["status"]; !ok {
		t.Fatalf("display 1 missing status")
	}
	if got, ok := display1["state_hash"].(string); !ok || got != "abc123" {
		t.Fatalf("display 1 state_hash=%v, want abc123", display1["state_hash"])
	}
}
