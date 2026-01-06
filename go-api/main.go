package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	mux := http.NewServeMux()

	// health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// API: GET /v1/display/{id}/config
	mux.HandleFunc("/v1/display/", handleDisplayConfig)

	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("go-api listening on", addr)
	log.Fatal(srv.ListenAndServe())
}

// ====== handlers ======

func handleDisplayConfig(w http.ResponseWriter, r *http.Request) {
	// expected path: /v1/display/{id}/config
	// very small router
	path := r.URL.Path

	// naive parse: "/v1/display/" + id + "/config"
	const prefix = "/v1/display/"
	if len(path) <= len(prefix) {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	rest := path[len(prefix):] // "{id}/config"
	// split by "/"
	var id string
	var tail string
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			id = rest[:i]
			tail = rest[i:]
			break
		}
	}
	if id == "" || tail != "/config" {
		http.Error(w, "use /v1/display/{id}/config", http.StatusBadRequest)
		return
	}

	directusURL := os.Getenv("DIRECTUS_URL")
	if directusURL == "" {
		directusURL = "http://cms:8055" // docker compose 내부 기본값
	}

	// 1) display 조회 (display_id로 필터)
	display, err := fetchOneDisplayByDisplayID(directusURL, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// 2) settings 조회(지금은 없을 수 있으니 null 가능)
	settings, _ := fetchSettings(directusURL)

	resp := map[string]any{
		"settings": settings, // nil이면 null
		"display":  display,
		// 다음 단계에서 display_routes 붙일 예정
		"display_routes": []any{},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ====== directus client ======

type directusList[T any] struct {
	Data []T `json:"data"`
}

type Display struct {
	ID        int    `json:"id"`
	DisplayID int    `json:"display_id"`
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type Settings map[string]any // 아직 스키마 확정 전이니 유연하게

func fetchOneDisplayByDisplayID(baseURL, displayID string) (*Display, error) {
	// Directus: /items/displays?filter[display_id][_eq]=1&limit=1
	url := baseURL + "/items/displays?filter[display_id][_eq]=" + displayID + "&limit=1"

	var out directusList[Display]
	if err := httpGetJSON(url, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, &httpError{status: http.StatusNotFound, msg: "display not found"}
	}
	return &out.Data[0], nil
}

func fetchSettings(baseURL string) (Settings, error) {
	// settings를 아직 안 만들었으면 404/empty일 수 있음
	// Singleton이든 아니든 일단 list로 받는다
	url := baseURL + "/items/settings?limit=1"

	var out directusList[map[string]any]
	if err := httpGetJSON(url, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	return out.Data[0], nil
}

func httpGetJSON(url string, target any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return &httpError{status: resp.StatusCode, msg: "directus returned " + resp.Status}
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }
