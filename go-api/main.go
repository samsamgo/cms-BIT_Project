package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// =========================
// Directus 응답 공통 구조
// =========================

type DirectusList[T any] struct {
	Data []T `json:"data"`
}

// =========================
// Data Models
// =========================

type Display struct {
	ID        int    `json:"id"`
	DisplayID int    `json:"display_id"`
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// =========================
// main
// =========================

func main() {
	mux := http.NewServeMux()

	// ---- Health Check ----
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// ---- GET /v1/display ----
	// Directus에서 displays 컬렉션 읽어서 그대로 반환
	mux.HandleFunc("/v1/display", func(w http.ResponseWriter, r *http.Request) {
		directusURL := os.Getenv("DIRECTUS_URL")
		if directusURL == "" {
			directusURL = "http://localhost:8055"
		}

		displays, err := fetchDisplays(directusURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(displays)
	})

	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("go-api listening on", addr)
	log.Fatal(srv.ListenAndServe())
}

// =========================
// Directus Client
// =========================

func fetchDisplays(directusURL string) ([]Display, error) {
	url := directusURL + "/items/displays"

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, &httpError{
			status: resp.StatusCode,
			msg:    "directus returned " + resp.Status,
		}
	}

	var result DirectusList[Display]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// =========================
// Error Helper
// =========================

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string {
	return e.msg
}
