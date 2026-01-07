package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type DirectusList[T any] struct {
	Data []T `json:"data"`
}

type Display struct {
	ID        int    `json:"id"`
	DisplayID int    `json:"display_id"`
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}
type Setting struct {
	ID          int    `json:"id"`
	Theme       string `json:"theme"`
	Refresh_Sec int    `json:"refresh_sec"`
	Font        string `json:"font"`
	Max_Routes  int    `json:"max_routes"`
}

type Displayconfig struct {
	Display Display `json:"display_id"`
	Setting Setting `json:"setting_id"`
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

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
		_ = json.NewEncoder(w).Encode(displays)
	})

	mux.HandleFunc("/v1/display/1/config", func(w http.ResponseWriter, r *http.Request) {
		directusURL := os.Getenv("DIRECTUS_URL")
		if directusURL == "" {
			directusURL = "http://localhost:8055"
		}

		settings, err := fetchSettings(directusURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
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
func fetchSettings(directusURL string) ([]Setting, error) {
	url := directusURL + "/items/settings"
	client := &http.Client{Timeout: 8 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	token := os.Getenv("DIRECTUS_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	log.Println("Directus GET:", url, "status:", resp.Status)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("directus returned %s: %s", resp.Status, string(body))
	}
	var result struct {
		Data []Setting `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil

}

func fetchDisplays(directusURL string) ([]Display, error) {
	url := directusURL + "/items/displays"

	client := &http.Client{Timeout: 8 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Bearer Token (절대 하드코딩하지 말고 환경변수로 받습니다)
	token := os.Getenv("DIRECTUS_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	log.Println("Directus GET:", url, "status:", resp.Status)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("directus returned %s: %s", resp.Status, string(body))
	}

	var result struct {
		Data []Display `json:"data"`
	}
	//var result DirectusList[Display]
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Data, nil
}
