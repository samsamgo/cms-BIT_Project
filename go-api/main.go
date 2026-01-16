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

type StateRoute struct {
	Route      string `json:"route"`
	DisplayETA string `json:"display_eta"`
	Status     string `json:"status"` // "OK" | "NO_DATA" | "ENDED"
}

type StateResponse struct {
	Theme      string       `json:"theme"`
	RefreshSec int          `json:"refresh_sec"`
	Notice     string       `json:"notice"`
	Routes     []StateRoute `json:"routes"`
}

type ETASnapshot struct {
	ETASec *int
	Ended  bool
}

// TAGO 응답이 item이 "객체"로 오거나 "배열"로 오는 경우가 있어서 둘 다 처리합니다.
type tagoArrivalResp struct {
	Response struct {
		Header struct {
			ResultCode string `json:"resultCode"`
			ResultMsg  string `json:"resultMsg"`
		} `json:"header"`
		Body struct {
			Items struct {
				Item json.RawMessage `json:"item"`
			} `json:"items"`
			TotalCount int `json:"totalCount"`
		} `json:"body"`
	} `json:"response"`
}

type tagoArrivalItem struct {
	ArrPrevStationCnt int    `json:"arrprevstationcnt"`
	ArrTime           int    `json:"arrtime"` // seconds
	NodeID            string `json:"nodeid"`
	NodeNm            string `json:"nodenm"`
	RouteID           string `json:"routeid"`
	RouteNo           int    `json:"routeno"`
	RouteTp           string `json:"routetp"`
	VehicleTp         string `json:"vehicletp"`
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
	// ✅ 캐시도 없는데 실패면, 이건 꽤 치명적 상태라 로그 남김
	addFailLog("writeCachedOrError/no_cache", err)
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

	mux.HandleFunc("/v1/display/1/state", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		hasCache := len(lastGoodRaw) > 0
		cacheMu.RUnlock()

		if !hasCache {
			_ = refreshCacheOnce()
		}

		_, rawState, err := buildStateFromCachedConfig(1)
		if err != nil {
			addFailLog("state_build", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rawState)
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
func getTagoServiceKey() string {
	return strings.TrimSpace(os.Getenv("TAGO_ARVL_SERVICE_KEY"))
}

func fetchArrivalsTAGO(cityCode int, nodeId string) (map[string]ETASnapshot, error) {
	key := getTagoServiceKey()
	if key == "" {
		return nil, fmt.Errorf("SERVICE_KEY is empty")
	}

	// 정류장 기준 도착예정정보
	// GET /1613000/ArvlInfoInqireService/getSttnAcctoArvlPrearngeInfoList
	u := fmt.Sprintf(
		"https://apis.data.go.kr/1613000/ArvlInfoInqireService/getSttnAcctoArvlPrearngeInfoList?serviceKey=%s&_type=json&cityCode=%d&nodeId=%s&numOfRows=30&pageNo=1",
		key, cityCode, nodeId,
	)

	req, _ := http.NewRequest("GET", u, nil)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tago request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tago error: %s", string(b))
	}

	var decoded tagoArrivalResp
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}

	// item 파싱(객체 or 배열)
	items := make([]tagoArrivalItem, 0)
	raw := decoded.Response.Body.Items.Item
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]ETASnapshot{}, nil
	}

	// 배열 시도
	var arr []tagoArrivalItem
	if err := json.Unmarshal(raw, &arr); err == nil {
		items = arr
	} else {
		// 객체 시도
		var one tagoArrivalItem
		if err2 := json.Unmarshal(raw, &one); err2 == nil {
			items = append(items, one)
		} else {
			return nil, fmt.Errorf("unexpected item format")
		}
	}

	// routeno -> ETA snapshot
	out := make(map[string]ETASnapshot, len(items))
	for _, it := range items {
		sec := it.ArrTime
		// arrtime이 0이거나 음수면 데이터 이상으로 보고 nil 처리
		if sec <= 0 {
			out[strconv.Itoa(it.RouteNo)] = ETASnapshot{ETASec: nil, Ended: false}
			continue
		}
		tmp := sec
		out[strconv.Itoa(it.RouteNo)] = ETASnapshot{ETASec: &tmp, Ended: false}
	}

	return out, nil
}

/* =====================
   Stage 3: State Builders
===================== */

// 표시 규칙:
// - ended=true -> "운행 종료", "ENDED"
// - eta=nil   -> "정보 없음", "NO_DATA"
// - eta<=60   -> "도착중",   "OK"
// - eta>60    -> "N분",      "OK"
func formatETA(etaSec *int, ended bool) (display string, status string) {
	const (
		ETA_NO_DATA  = "정보 없음"
		ETA_ARRIVING = "도착중"
		ETA_ENDED    = "운행 종료"
	)
	if ended {
		return "운행 종료", "ENDED"
	}
	if etaSec == nil {
		return "정보 없음", "NO_DATA"
	}
	if *etaSec <= 60 {
		return "도착중", "OK"
	}
	// 기본: 올림(125초 -> 3분)
	min := (*etaSec + 59) / 60
	return fmt.Sprintf("%d분", min), "OK"
}

func limitRoutes(in []StateRoute, max int) []StateRoute {
	if max <= 0 {
		return []StateRoute{}
	}
	if len(in) <= max {
		return in
	}
	return in[:max]
}

func clampText(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= maxLen {
		return s
	}
	// 길이 제한: "…" 붙이기(최소 2 이상일 때)
	if maxLen == 1 {
		return string(rs[:1])
	}
	return string(rs[:maxLen-1]) + "…"
}

// 더미 ETA 생성 규칙(테스트용):
// - routeID%7==0 -> ended
// - routeID%5==0 -> no data
// - 그 외 -> 20~260초 사이 값
func getDummyETA(routeID int) ETASnapshot {
	if routeID%7 == 0 {
		return ETASnapshot{ETASec: nil, Ended: true}
	}
	if routeID%5 == 0 {
		return ETASnapshot{ETASec: nil, Ended: false}
	}
	sec := 20 + (routeID%9)*30 // 20,50,80,...,260
	return ETASnapshot{ETASec: &sec, Ended: false}
}

// state marshal이 실패하면 그 자체가 이상하므로, 여기서는 최소 JSON을 리턴
func mustMarshalState(st StateResponse) []byte {
	b, err := json.Marshal(st)
	if err != nil {
		return []byte(`{"theme":"default","refresh_sec":30,"notice":"marshal_failed","routes":[]}`)
	}
	return b
}

// cached config(lastGoodRaw)를 입력으로 state를 만든다.
// 어떤 경우에도 state 스키마는 깨지면 안 된다.
func buildStateFromCachedConfig(displayID int) (StateResponse, []byte, error) {
	cacheMu.RLock()
	rawCfg := append([]byte(nil), lastGoodRaw...)
	cacheMu.RUnlock()

	if len(rawCfg) == 0 {
		st := StateResponse{
			Theme:      "default",
			RefreshSec: 30,
			Notice:     "cache not ready",
			Routes:     []StateRoute{},
		}
		return st, mustMarshalState(st), fmt.Errorf("cache not ready")
	}

	var cfg DisplayConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		st := StateResponse{
			Theme:      "default",
			RefreshSec: 30,
			Notice:     "invalid cached config",
			Routes:     []StateRoute{},
		}
		return st, mustMarshalState(st), err
	}

	// display_id 최소 확인
	if cfg.Display.DisplayID != displayID {
		st := StateResponse{
			Theme:      cfg.Settings.Theme,
			RefreshSec: cfg.Settings.RefreshSec,
			Notice:     "display_id mismatch",
			Routes:     []StateRoute{},
		}
		return st, mustMarshalState(st), fmt.Errorf("display_id mismatch: got %d", cfg.Display.DisplayID)
	}

	out := make([]StateRoute, 0, len(cfg.Routes))
	// ✅ 실제 TAGO 도착정보 가져오기 (대전, 용운마젤란아파트)
	arrMap, arrErr := fetchArrivalsTAGO(25, "DJB8002304")
	if arrErr != nil {
		addFailLog("tago_arrivals", arrErr)
	}
	// 정책: enabled=true만 표시
	for _, r := range cfg.Routes {
		if !r.Enabled {
			continue
		}

		var eta ETASnapshot
		// route_name을 버스번호("605","608")로 쓰는 정책
		busNo := strings.TrimSpace(r.RouteName)

		if arrErr != nil {
			// TAGO 실패면 더미로 폴백(테스트 유지)
			eta = getDummyETA(r.RouteID)
		} else {
			if snap, ok := arrMap[busNo]; ok {
				eta = snap
			} else {
				eta = ETASnapshot{ETASec: nil, Ended: false} // 정보 없음
			}
		}
		displayETA, status := formatETA(eta.ETASec, eta.Ended)

		out = append(out, StateRoute{
			Route:      clampText(r.RouteName, 12),
			DisplayETA: displayETA,
			Status:     status,
		})
	}

	out = limitRoutes(out, cfg.Settings.MaxRoutes)

	state := StateResponse{
		Theme:      cfg.Settings.Theme,
		RefreshSec: cfg.Settings.RefreshSec,
		Notice:     "",
		Routes:     out,
	}

	rawState, err := json.Marshal(state)
	if err != nil {
		st := StateResponse{
			Theme:      "default",
			RefreshSec: 30,
			Notice:     "state marshal failed",
			Routes:     []StateRoute{},
		}
		return st, mustMarshalState(st), err
	}

	return state, rawState, nil
}

func isDummyMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("DUMMY_MODE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func buildDummyConfig() (DisplayConfig, []byte, error) {
	cfg := DisplayConfig{
		Display: Display{
			ID:        0,
			DisplayID: 1,
			Name:      "DUMMY Display",
			Width:     320,
			Height:    160,
		},
		Settings: Setting{
			ID:         0,
			Theme:      "dummy",
			RefreshSec: 30,
			Font:       "NOTO Sans KR",
			MaxRoutes:  5,
		},
		Routes: []struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}{
			{RouteID: 1, RouteName: "DUMMY-A", Enabled: true, SortOrder: 1},
			{RouteID: 2, RouteName: "DUMMY-B", Enabled: true, SortOrder: 2},
		},
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return DisplayConfig{}, nil, err
	}
	return cfg, raw, nil
}

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
	// ✅ 더미 모드면 Directus를 아예 안 침
	if isDummyMode() {
		_, raw, err := buildDummyConfig()
		if err != nil {
			addFailLog("dummy_build", err)
			return err
		}

		cacheMu.Lock()
		lastGoodRaw = raw
		lastGoodAt = time.Now()
		lastErr = ""
		cacheMu.Unlock()
		return nil
	}
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
