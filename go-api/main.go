package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

/* =====================
   Types
===================== */

type Display struct {
	ID        int    `json:"id"`
	DisplayID int    `json:"display_id"`
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type Setting struct {
	ID         int    `json:"id"`
	Theme      string `json:"theme"`
	RefreshSec int    `json:"refresh_sec"`
	Font       string `json:"font"`
	MaxRoutes  int    `json:"max_routes"`
}

type Route struct {
	ID        int    `json:"id"`
	RouteID   int    `json:"route_id"`
	RouteName string `json:"route_name"`
	Enabled   bool   `json:"enabled"`
}

type DisplayRoute struct {
	ID        int `json:"id"`
	DisplayID int `json:"display_id"`
	RouteID   int `json:"route_id"`
	SortOrder int `json:"sort_order"`
}

type DisplayConfig struct {
	Display  Display `json:"display"`
	Settings Setting `json:"settings"`
	Routes   []struct {
		RouteID   int    `json:"route_id"`
		RouteName string `json:"route_name"`
		Enabled   bool   `json:"enabled"`
		SortOrder int    `json:"sort_order"`
	} `json:"routes"`
}
type FailLog struct {
	At     time.Time `json:"at"`
	Where  string    `json:"where"`
	Reason string    `json:"reason"`
}

/* =====================
   Cache (last_good_raw)
===================== */

var (
	cacheMu     sync.RWMutex
	lastGoodRaw []byte
	lastGoodAt  time.Time
	lastErr     string
	// ✅ 실패 로그: 최근 50개만 유지
	failLogs    []FailLog
	failLogsMax = 50
)

func writeCachedOrError(w http.ResponseWriter, err error) {
	cacheMu.RLock()
	cached := append([]byte(nil), lastGoodRaw...)
	cacheMu.RUnlock()
	cacheMu.Lock()
	lastErr = err.Error()
	cacheMu.Unlock()

	if len(cached) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cached)
		return
	}

	// 캐시가 없을 때만 503 허용 (빈 JSON 금지)
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}

/* =====================
   Main
===================== */

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		ok := len(lastGoodRaw) > 0
		at := lastGoodAt
		le := lastErr

		// 최근 5개만 노출
		n := 5
		if len(failLogs) < n {
			n = len(failLogs)
		}
		recent := make([]FailLog, n)
		copy(recent, failLogs[len(failLogs)-n:])
		cacheMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"cache_ready":  ok,
			"cache_at":     at.Format(time.RFC3339),
			"last_error":   le,
			"recent_fails": recent,
		})
	})

	mux.HandleFunc("/v1/display/1/config", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		cached := append([]byte(nil), lastGoodRaw...)
		cacheMu.RUnlock()

		if len(cached) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cached)
			return
		}

		// 캐시가 아직 없으면 1회 강제 갱신 시도
		if err := refreshCacheOnce(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		cacheMu.RLock()
		cached = append([]byte(nil), lastGoodRaw...)
		cacheMu.RUnlock()

		if len(cached) == 0 {
			http.Error(w, "cache not ready", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cached)
	})

	// ✅ 시작할 때 1번 즉시 갱신 시도(캐시 채우기)
	_ = refreshCacheOnce()

	// ✅ 30초마다 갱신
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			_ = refreshCacheOnce()
		}
	}()

	log.Println("go-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

/*
	=====================
	  Fetch functions

=====================
*/
func addFailLog(where string, err error) {
	if err == nil {
		return
	}
	fl := FailLog{
		At:     time.Now(),
		Where:  where,
		Reason: err.Error(),
	}

	cacheMu.Lock()
	lastErr = fl.Reason

	failLogs = append(failLogs, fl)
	if len(failLogs) > failLogsMax {
		failLogs = failLogs[len(failLogs)-failLogsMax:]
	}
	cacheMu.Unlock()
}

func validateConfig(cfg DisplayConfig) error {
	// display 검증
	if cfg.Display.DisplayID != 1 {
		return fmt.Errorf("invalid display_id: %d", cfg.Display.DisplayID)
	}
	if cfg.Display.Width <= 0 || cfg.Display.Height <= 0 {
		return fmt.Errorf("invalid display size: %dx%d", cfg.Display.Width, cfg.Display.Height)
	}

	// settings 검증
	if cfg.Settings.Theme == "" {
		return fmt.Errorf("settings.theme is empty")
	}
	if cfg.Settings.RefreshSec <= 0 {
		return fmt.Errorf("settings.refresh_sec must be > 0")
	}
	if cfg.Settings.Font == "" {
		return fmt.Errorf("settings.font is empty")
	}
	if cfg.Settings.MaxRoutes <= 0 {
		return fmt.Errorf("settings.max_routes must be > 0")
	}

	// routes 검증 (정책: 비어있으면 실패)
	if len(cfg.Routes) == 0 {
		return fmt.Errorf("routes is empty")
	}
	for i, r := range cfg.Routes {
		if r.RouteID <= 0 {
			return fmt.Errorf("routes[%d].route_id must be > 0", i)
		}
		if r.RouteName == "" {
			return fmt.Errorf("routes[%d].route_name is empty", i)
		}
		if r.SortOrder <= 0 {
			return fmt.Errorf("routes[%d].sort_order must be > 0", i)
		}
	}
	return nil
}

func getDirectusURL() string {
	//Directus URL을 얻는 함수 추가
	directusURL := os.Getenv("DIRECTUS_URL")
	if directusURL == "" {
		directusURL = "http://localhost:8055"
	}
	return directusURL
}

func refreshCacheOnce() error {
	//ticker가 이 함수를 주기적으로 부릅니다.
	directusURL := getDirectusURL()

	_, raw, err := buildConfig(directusURL)
	if err != nil {
		cacheMu.Lock()
		lastErr = err.Error()
		cacheMu.Unlock()
		return err
	}

	cacheMu.Lock()
	lastGoodRaw = raw
	lastGoodAt = time.Now()
	lastErr = ""
	cacheMu.Unlock()

	return nil
}

func buildConfig(directusURL string) (DisplayConfig, []byte, error) {
	// 1) settings
	settings, err := fetchSettings(directusURL)
	if err != nil {
		return DisplayConfig{}, nil, err
	}

	// 2) display
	displays, err := fetchDisplays(directusURL)
	if err != nil {
		return DisplayConfig{}, nil, err
	}

	var display Display
	found := false
	for _, d := range displays {
		if d.DisplayID == 1 {
			display = d
			found = true
			break
		}
	}
	if !found {
		return DisplayConfig{}, nil, fmt.Errorf("display_id=1 not found")
	}

	// 3) display_routes
	links, err := fetchDisplayRoutes(directusURL, 1)
	if err != nil {
		return DisplayConfig{}, nil, err
	}

	routeIDs := make([]int, 0, len(links))
	sortMap := make(map[int]int)
	for _, l := range links {
		routeIDs = append(routeIDs, l.RouteID)
		sortMap[l.RouteID] = l.SortOrder
	}

	// 4) routes
	routes, err := fetchRoutesByIDs(directusURL, routeIDs)
	if err != nil {
		return DisplayConfig{}, nil, err
	}

	routeMap := make(map[int]Route)
	for _, rr := range routes {
		routeMap[rr.RouteID] = rr
	}

	cfgRoutes := make([]struct {
		RouteID   int    `json:"route_id"`
		RouteName string `json:"route_name"`
		Enabled   bool   `json:"enabled"`
		SortOrder int    `json:"sort_order"`
	}, 0)

	for _, rid := range routeIDs {
		rr, ok := routeMap[rid]
		if !ok {
			continue
		}
		cfgRoutes = append(cfgRoutes, struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}{
			RouteID:   rr.RouteID,
			RouteName: rr.RouteName,
			Enabled:   rr.Enabled,
			SortOrder: sortMap[rid],
		})
	}

	cfg := DisplayConfig{
		Display:  display,
		Settings: settings,
		Routes:   cfgRoutes,
	}

	// ✅ 스키마 검증: 실패면 캐시 갱신 금지
	if err := validateConfig(cfg); err != nil {
		return DisplayConfig{}, nil, err
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return DisplayConfig{}, nil, err
	}

	return cfg, raw, nil
}

func authRequest(req *http.Request) {
	token := os.Getenv("DIRECTUS_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func fetchSettings(directusURL string) (Setting, error) {
	url := directusURL + "/items/settings"
	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	// NOTE: timeout≤5초 적용(기본 client로는 무한대 가능)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return Setting{}, fmt.Errorf("directus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return Setting{}, fmt.Errorf("directus error: %s", string(b))
	}

	var result struct {
		Data Setting `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, err
}

func fetchDisplays(directusURL string) ([]Display, error) {
	url := directusURL + "/items/displays"
	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("directus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("directus error: %s", string(b))
	}

	var result struct {
		Data []Display `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, err
}

func fetchDisplayRoutes(directusURL string, displayID int) ([]DisplayRoute, error) {
	url := fmt.Sprintf("%s/items/display_routes?filter[display_id][_eq]=%d&sort=sort_order", directusURL, displayID)
	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("directus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("directus error: %s", string(b))
	}

	var result struct {
		Data []DisplayRoute `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, err
}

func fetchRoutesByIDs(directusURL string, ids []int) ([]Route, error) {
	if len(ids) == 0 {
		return []Route{}, nil
	}

	s := make([]string, 0, len(ids))
	for _, id := range ids {
		s = append(s, strconv.Itoa(id))
	}
	in := strings.Join(s, ",")

	url := fmt.Sprintf("%s/items/routes?filter[route_id][_in]=%s", directusURL, in)
	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("directus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("directus error: %s", string(b))
	}

	var result struct {
		Data []Route `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, err
}
