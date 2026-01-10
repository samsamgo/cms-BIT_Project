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
		for _, r := range routes {
			routeMap[r.RouteID] = r
		}

		cfgRoutes := make([]struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}, 0)

		for _, rid := range routeIDs {
			r, ok := routeMap[rid]
			if !ok {
				continue
			}
			cfgRoutes = append(cfgRoutes, struct {
				RouteID   int    `json:"route_id"`
				RouteName string `json:"route_name"`
				Enabled   bool   `json:"enabled"`
				SortOrder int    `json:"sort_order"`
			}{
				RouteID:   r.RouteID,
				RouteName: r.RouteName,
				Enabled:   r.Enabled,
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

func fetchSettings(directusURL string) (Setting, error) {
	url := directusURL + "/items/settings"
	req, _ := http.NewRequest("GET", url, nil)
	authRequest(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Setting{}, err
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []Route `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result.Data, err
}
