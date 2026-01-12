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
   HTTP Client (timeout <= 5s)
===================== */

// 전역으로 둬야 doGet/fetch*에서 쓸 수 있음
var httpClient = &http.Client{
	Timeout: 5 * time.Second,
}

/* =====================
   Main
===================== */

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/v1/display/1/config", func(w http.ResponseWriter, r *http.Request) {
		directusURL := os.Getenv("DIRECTUS_URL")
		if directusURL == "" {
			directusURL = "http://localhost:8055"
		}

		// 1. settings
		settings, err := fetchSettings(directusURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// 2. display
		displays, err := fetchDisplays(directusURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
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
			http.Error(w, "display_id=1 not found", http.StatusNotFound)
			return
		}

		// 3. display_routes
		links, err := fetchDisplayRoutes(directusURL, 1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		routeIDs := make([]int, 0, len(links))
		sortMap := make(map[int]int)
		for _, l := range links {
			routeIDs = append(routeIDs, l.RouteID)
			sortMap[l.RouteID] = l.SortOrder
		}

		// 4. routes
		routes, err := fetchRoutesByIDs(directusURL, routeIDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
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

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	})

	log.Println("go-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

/* =====================
   Fetch functions
===================== */

func authRequest(req *http.Request) {
	token := os.Getenv("DIRECTUS_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// 공통 외부 API GET (raw bytes 반환)
func doGet(directusURL, pathWithQuery string) ([]byte, error) {
	url := directusURL + pathWithQuery

	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("directus request failed: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}

	if resp.StatusCode >= 300 {
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("directus error status=%d body=%s", resp.StatusCode, msg)
	}

	return b, nil
}

func fetchSettings(directusURL string) (Setting, error) {
	raw, err := doGet(directusURL, "/items/settings")
	if err != nil {
		return Setting{}, err
	}

	var result struct {
		Data Setting `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return Setting{}, fmt.Errorf("decode settings failed: %w", err)
	}
	return result.Data, nil
}

func fetchDisplays(directusURL string) ([]Display, error) {
	raw, err := doGet(directusURL, "/items/displays")
	if err != nil {
		return nil, err
	}

	var result struct {
		Data []Display `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode displays failed: %w", err)
	}
	return result.Data, nil
}

func fetchDisplayRoutes(directusURL string, displayID int) ([]DisplayRoute, error) {
	path := fmt.Sprintf("/items/display_routes?filter[display_id][_eq]=%d&sort=sort_order", displayID)
	raw, err := doGet(directusURL, path)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data []DisplayRoute `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode display_routes failed: %w", err)
	}
	return result.Data, nil
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

	path := fmt.Sprintf("/items/routes?filter[route_id][_in]=%s", in)
	raw, err := doGet(directusURL, path)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data []Route `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode routes failed: %w", err)
	}
	return result.Data, nil
}
