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

/* =====================
   Cache (last_good_raw)
===================== */

var (
	cacheMu     sync.RWMutex
	lastGoodRaw []byte
	lastGoodAt  time.Time
	lastErr     string
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
		cacheMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"cache_ready": ok,
			"cache_at":    at.Format(time.RFC3339),
			"last_error":  le,
		})
	})

	mux.HandleFunc("/v1/display/1/config", func(w http.ResponseWriter, r *http.Request) {
		// ✅ directusURL을 여기서 잡아야 함
		directusURL := os.Getenv("DIRECTUS_URL")
		if directusURL == "" {
			directusURL = "http://localhost:8055"
		}

		cfg, raw, err := buildConfig(directusURL)
		_ = cfg // (지금은 raw만 쓰지만, cfg가 필요해질 수도 있으니 유지)

		if err != nil {
			// ✅ 실패 기록
			cacheMu.Lock()
			lastErr = err.Error()
			cacheMu.Unlock()

			writeCachedOrError(w, err)
			return
		}

		// ✅ 성공이면 캐시 갱신 + lastErr 초기화
		cacheMu.Lock()
		lastGoodRaw = raw
		lastGoodAt = time.Now()
		lastErr = ""
		cacheMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})

	log.Println("go-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

/* =====================
   Fetch functions
===================== */

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
