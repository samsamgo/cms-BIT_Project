package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
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

// TAGO 응답 item이 "객체" 또는 "배열"로 올 수 있어 RawMessage로 분기
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
   Cache (Directus config)
===================== */

var (
	cacheMu     sync.RWMutex
	lastGoodRaw []byte
	lastGoodAt  time.Time
	lastErr     string

	// 최근 50개 실패 로그
	failLogs    []FailLog
	failLogsMax = 50
)

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

/* =====================
   TAGO Cache (BIT급) - /state 요청마다 TAGO 치지 않음
===================== */

var (
	tagoMu       sync.RWMutex
	tagoLastMap  map[string]ETASnapshot
	tagoLastAt   time.Time
	tagoLastErr  string
	tagoLastKeys []string // 디버깅 편의
)

func setTagoCache(m map[string]ETASnapshot, err error) {
	tagoMu.Lock()
	defer tagoMu.Unlock()

	if err != nil {
		tagoLastErr = err.Error()
		// 실패해도 기존 캐시는 유지 (죽지 말고 흐리게)
		return
	}

	tagoLastMap = m
	tagoLastAt = time.Now()
	tagoLastErr = ""

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tagoLastKeys = keys
}

func getTagoCacheCopy() (map[string]ETASnapshot, time.Time, string, []string) {
	tagoMu.RLock()
	defer tagoMu.RUnlock()

	out := make(map[string]ETASnapshot, len(tagoLastMap))
	for k, v := range tagoLastMap {
		out[k] = v
	}
	keys := append([]string(nil), tagoLastKeys...)
	return out, tagoLastAt, tagoLastErr, keys
}

/* =====================
   Render Pipeline (state -> PNG)
===================== */

var (
	renderMu      sync.Mutex
	renderRunning bool
	lastStateHash string
)

func getenvDefault(k, def string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v
}

func getenvIntDefault(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func atomicWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func callRendererPNG(rendererURL string, width, height int, st StateResponse) ([]byte, error) {
	payload := map[string]any{
		"width":  width,
		"height": height,
		"state":  st,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", rendererURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("renderer request failed: %w", err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("renderer status=%d body=%s", resp.StatusCode, string(b))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "image/png") {
		return nil, fmt.Errorf("renderer invalid content-type=%s", ct)
	}
	return b, nil
}

func getCachedDisplaySizeFallback() (int, int) {
	cacheMu.RLock()
	rawCfg := append([]byte(nil), lastGoodRaw...)
	cacheMu.RUnlock()

	// 기본값
	w, h := 320, 160
	if len(rawCfg) == 0 {
		return w, h
	}

	var cfg DisplayConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return w, h
	}
	if cfg.Display.Width > 0 {
		w = cfg.Display.Width
	}
	if cfg.Display.Height > 0 {
		h = cfg.Display.Height
	}
	return w, h
}

func startRenderRefresher() {
	// renderer-service의 POST /render 를 직접 가리키도록 통일
	// 예: http://renderer:3000/render
	rendererURL := getenvDefault("RENDERER_URL", "http://renderer:3000/render")
	screenPath := getenvDefault("SCREEN_PATH", "/data/screen.png")

	// env로 주기 강제 가능
	overrideSec := getenvIntDefault("RENDER_INTERVAL_SEC", 0)

	go func() {
		// 첫 렌더는 빠르게(부팅 직후 화면 준비)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			// 동시 렌더 방지
			renderMu.Lock()
			if renderRunning {
				renderMu.Unlock()
				continue
			}
			renderRunning = true
			renderMu.Unlock()

			func() {
				defer func() {
					renderMu.Lock()
					renderRunning = false
					renderMu.Unlock()
				}()

				// state 생성(캐시 없으면 refresh 시도)
				cacheMu.RLock()
				hasCache := len(lastGoodRaw) > 0
				cacheMu.RUnlock()
				if !hasCache {
					_ = refreshCacheOnce()
				}

				st, _, err := buildStateFromCachedConfig(1)
				if err != nil {
					addFailLog("render_state_build", err)
					// 실패해도 기존 screen.png 유지
					return
				}

				// 주기 결정: env override > state.refresh_sec > 5
				intervalSec := 5
				if overrideSec > 0 {
					intervalSec = overrideSec
				} else if st.RefreshSec > 0 {
					intervalSec = st.RefreshSec
				}
				if intervalSec < 1 {
					intervalSec = 1
				}
				ticker.Reset(time.Duration(intervalSec) * time.Second)

				// display size (Directus 우선, fallback 320x160)
				w, h := getCachedDisplaySizeFallback()

				// 동일 state면 렌더 스킵(선택 기능)
				rawForHash, err := json.Marshal(st)
				if err == nil {
					hv := sha256Hex(rawForHash)
					if hv == lastStateHash {
						return
					}
					lastStateHash = hv
				}

				png, err := callRendererPNG(rendererURL, w, h, st)
				if err != nil {
					addFailLog("renderer_call", err)
					// 실패 시 기존 screen.png 유지
					return
				}
				if err := atomicWriteFile(screenPath, png); err != nil {
					addFailLog("screen_write", err)
					return
				}
			}()
		}
	}()
}

/* =====================
   Utils
===================== */

func clampText(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return string(rs[:1])
	}
	return string(rs[:maxLen-1]) + "…"
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

func normalizeRouteKey(s string) string {
	// 매칭용 정규화: 공백 제거 + "번" 제거
	x := strings.TrimSpace(s)
	x = strings.ReplaceAll(x, " ", "")
	x = strings.TrimSuffix(x, "번")
	return x
}

// state marshal 실패 시에도 스키마 깨지면 안 됨
func mustMarshalState(st StateResponse) []byte {
	b, err := json.Marshal(st)
	if err != nil {
		return []byte(`{"theme":"default","refresh_sec":30,"notice":"marshal_failed","routes":[]}`)
	}
	return b
}

/* =====================
   ENV helpers
===================== */

func getTagoServiceKey() string {
	return strings.TrimSpace(os.Getenv("TAGO_ARVL_SERVICE_KEY"))
}

func getTagoCityNode() (int, string, error) {
	cityStr := strings.TrimSpace(os.Getenv("TAGO_CITY_CODE"))
	nodeID := strings.TrimSpace(os.Getenv("TAGO_NODE_ID"))

	if cityStr == "" {
		return 0, "", fmt.Errorf("TAGO_CITY_CODE is empty")
	}
	cityCode, err := strconv.Atoi(cityStr)
	if err != nil || cityCode <= 0 {
		return 0, "", fmt.Errorf("TAGO_CITY_CODE invalid: %q", cityStr)
	}
	if nodeID == "" {
		return 0, "", fmt.Errorf("TAGO_NODE_ID is empty")
	}
	return cityCode, nodeID, nil
}

func allowDummyETA() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_DUMMY_ETA")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func loadDotEnvBestEffort() {
	// go-api에서 실행하는 경우가 많아서 ../.env 우선
	if _, err := os.Stat("../.env"); err == nil {
		_ = godotenv.Load("../.env")
		return
	}
	// 같은 폴더에 .env가 있으면 로드
	if _, err := os.Stat(".env"); err == nil {
		_ = godotenv.Load(".env")
		return
	}
	// 없으면 그냥 무시
}

/* =====================
   TAGO fetch
===================== */

func fetchArrivalsTAGO(cityCode int, nodeId string) (map[string]ETASnapshot, error) {
	key := getTagoServiceKey()
	if key == "" {
		// ✅ 에러 문구 명확화
		return nil, fmt.Errorf("TAGO_ARVL_SERVICE_KEY is empty")
	}

	base := "https://apis.data.go.kr/1613000/ArvlInfoInqireService/getSttnAcctoArvlPrearngeInfoList"
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid tago base url: %w", err)
	}
	q := u.Query()
	q.Set("serviceKey", key)
	q.Set("_type", "json")
	q.Set("cityCode", strconv.Itoa(cityCode))
	q.Set("nodeId", nodeId)
	q.Set("numOfRows", "30")
	q.Set("pageNo", "1")
	u.RawQuery = q.Encode()

	req, _ := http.NewRequest("GET", u.String(), nil)

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

	out := make(map[string]ETASnapshot, len(items))
	for _, it := range items {
		sec := it.ArrTime
		k := strconv.Itoa(it.RouteNo)

		// arrtime <= 0은 유효하지 않은 데이터로 보고 NO_DATA 처리
		if sec <= 0 {
			out[k] = ETASnapshot{ETASec: nil, Ended: false}
			continue
		}
		tmp := sec
		out[k] = ETASnapshot{ETASec: &tmp, Ended: false}
	}
	return out, nil
}

/* =====================
   Stage 3: State Builders
===================== */

// 표시 규칙(문구 상수화):
// - ended=true -> "운행 종료", "ENDED"
// - eta=nil -> "정보 없음", "NO_DATA"
// - eta<=60 -> "도착중", "OK"
// - eta>60 -> "N분", "OK"
func formatETA(etaSec *int, ended bool) (display string, status string) {
	if ended {
		return "운행 종료", "ENDED"
	}
	if etaSec == nil {
		return "정보 없음", "NO_DATA"
	}
	if *etaSec <= 60 {
		return "도착중", "OK"
	}
	min := (*etaSec + 59) / 60
	return fmt.Sprintf("%d분", min), "OK"
}

// 더미 ETA(개발 편의용) - 운영에서는 기본 금지
func getDummyETA(routeID int) ETASnapshot {
	if routeID%7 == 0 {
		return ETASnapshot{ETASec: nil, Ended: true}
	}
	if routeID%5 == 0 {
		return ETASnapshot{ETASec: nil, Ended: false}
	}
	sec := 20 + (routeID%9)*30
	return ETASnapshot{ETASec: &sec, Ended: false}
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

	// display_id 확인(하드 실패로 막지 말고 notice로만 남김)
	mismatch := (displayID > 0 && cfg.Display.DisplayID != displayID)

	// TAGO 캐시 사용 (요청마다 fetch 금지)
	arrMap, _, tagoErr, _ := getTagoCacheCopy()

	noticeParts := make([]string, 0, 2)
	if mismatch {
		noticeParts = append(noticeParts, "display_id mismatch")
	}
	if tagoErr != "" {
		noticeParts = append(noticeParts, "실시간 조회 실패")
	}
	notice := strings.Join(noticeParts, " | ")

	out := make([]StateRoute, 0, len(cfg.Routes))

	for _, r := range cfg.Routes {
		if !r.Enabled {
			continue
		}

		// route_name을 버스번호("605","608")로 쓰는 정책
		busNo := normalizeRouteKey(r.RouteName)

		var eta ETASnapshot
		if tagoErr != "" {
			// ✅ 운영 기본: 더미 금지 → NO_DATA
			if allowDummyETA() {
				eta = getDummyETA(r.RouteID)
			} else {
				eta = ETASnapshot{ETASec: nil, Ended: false}
			}
		} else {
			if snap, ok := arrMap[busNo]; ok {
				eta = snap
			} else {
				eta = ETASnapshot{ETASec: nil, Ended: false}
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
		Notice:     notice,
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

/* =====================
   Dummy Mode (Directus)
===================== */

func isDummyMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("DUMMY_MODE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func buildDummyConfig() (DisplayConfig, []byte, error) {
	cfg := DisplayConfig{
		Display: Display{
			ID:        2,
			DisplayID: 1,
			Name:      "Test Display",
			Width:     320,
			Height:    160,
		},
		Settings: Setting{
			ID:         1,
			Theme:      "default",
			RefreshSec: 5,
			Font:       "NOTO Sans KR",
			MaxRoutes:  5,
		},
		Routes: []struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}{
			{RouteID: 10, RouteName: "A노선", Enabled: true, SortOrder: 1},
			{RouteID: 12, RouteName: "B노선", Enabled: true, SortOrder: 2},
		},
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, raw, nil
}

/* =====================
   Directus fetch (Stage 1/2)
===================== */

type directusDisplayItem struct {
	ID        int    `json:"id"`
	DisplayID int    `json:"display_id"`
	Name      string `json:"name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type directusSettingItem struct {
	ID         int    `json:"id"`
	Theme      string `json:"theme"`
	RefreshSec int    `json:"refresh_sec"`
	Font       string `json:"font"`
	MaxRoutes  int    `json:"max_routes"`
}

type directusRouteItem struct {
	ID        int    `json:"id"`
	RouteID   int    `json:"route_id"`
	RouteName string `json:"route_name"`
	Enabled   bool   `json:"enabled"`
}

type directusDisplayRouteItem struct {
	ID        int `json:"id"`
	DisplayID int `json:"display_id"`
	RouteID   int `json:"route_id"`
	SortOrder int `json:"sort_order"`
}

type directusListResp[T any] struct {
	Data []T `json:"data"`
}

func getDirectusBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("DIRECTUS_URL")), "/")
}

func fetchJSON(url string, out any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("directus error %d: %s", resp.StatusCode, string(b))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// Directus에서 display(1) + setting(1) + display_routes + routes 조합해서 최종 config JSON을 만든다.
func fetchConfigFromDirectus(displayID int) (DisplayConfig, []byte, error) {
	if isDummyMode() {
		return buildDummyConfig()
	}

	base := getDirectusBaseURL()
	if base == "" {
		return DisplayConfig{}, nil, fmt.Errorf("DIRECTUS_URL is empty")
	}

	// 1) display
	var dispResp directusListResp[directusDisplayItem]
	if err := fetchJSON(fmt.Sprintf("%s/items/displays?filter[display_id][_eq]=%d&limit=1", base, displayID), &dispResp); err != nil {
		return DisplayConfig{}, nil, err
	}
	if len(dispResp.Data) == 0 {
		return DisplayConfig{}, nil, fmt.Errorf("display not found for display_id=%d", displayID)
	}
	disp := dispResp.Data[0]

	// 2) setting (display_id 기준이 아니라 그냥 1개만 쓰는 정책이면 limit=1)
	//    (기존 프로젝트 정책 유지)
	var setResp directusListResp[directusSettingItem]
	if err := fetchJSON(fmt.Sprintf("%s/items/settings?limit=1", base), &setResp); err != nil {
		return DisplayConfig{}, nil, err
	}
	if len(setResp.Data) == 0 {
		return DisplayConfig{}, nil, fmt.Errorf("settings not found")
	}
	set := setResp.Data[0]

	// 3) display_routes (display_id로 필터)
	var drResp directusListResp[directusDisplayRouteItem]
	if err := fetchJSON(fmt.Sprintf("%s/items/display_routes?filter[display_id][_eq]=%d&limit=100", base, displayID), &drResp); err != nil {
		return DisplayConfig{}, nil, err
	}

	// 4) routes 전체(또는 필요한 것만)
	var rResp directusListResp[directusRouteItem]
	if err := fetchJSON(fmt.Sprintf("%s/items/routes?limit=500", base), &rResp); err != nil {
		return DisplayConfig{}, nil, err
	}

	routeMap := make(map[int]directusRouteItem, len(rResp.Data))
	for _, r := range rResp.Data {
		routeMap[r.RouteID] = r
	}

	// display_routes + routes merge
	type mergedRoute struct {
		RouteID   int    `json:"route_id"`
		RouteName string `json:"route_name"`
		Enabled   bool   `json:"enabled"`
		SortOrder int    `json:"sort_order"`
	}

	merged := make([]mergedRoute, 0, len(drResp.Data))
	for _, dr := range drResp.Data {
		ri, ok := routeMap[dr.RouteID]
		if !ok {
			continue
		}
		merged = append(merged, mergedRoute{
			RouteID:   ri.RouteID,
			RouteName: ri.RouteName,
			Enabled:   ri.Enabled,
			SortOrder: dr.SortOrder,
		})
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].SortOrder < merged[j].SortOrder
	})

	cfg := DisplayConfig{
		Display: Display{
			ID:        disp.ID,
			DisplayID: disp.DisplayID,
			Name:      disp.Name,
			Width:     disp.Width,
			Height:    disp.Height,
		},
		Settings: Setting{
			ID:         set.ID,
			Theme:      set.Theme,
			RefreshSec: set.RefreshSec,
			Font:       set.Font,
			MaxRoutes:  set.MaxRoutes,
		},
		Routes: make([]struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}, 0, len(merged)),
	}

	for _, mr := range merged {
		cfg.Routes = append(cfg.Routes, struct {
			RouteID   int    `json:"route_id"`
			RouteName string `json:"route_name"`
			Enabled   bool   `json:"enabled"`
			SortOrder int    `json:"sort_order"`
		}{
			RouteID:   mr.RouteID,
			RouteName: mr.RouteName,
			Enabled:   mr.Enabled,
			SortOrder: mr.SortOrder,
		})
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, raw, nil
}

/* =====================
   Cache Refresh (Directus)
===================== */

func refreshCacheOnce() error {
	cfg, raw, err := fetchConfigFromDirectus(1)
	if err != nil {
		addFailLog("directus_fetch", err)
		return err
	}

	_ = cfg // (현재는 raw만 캐시)

	cacheMu.Lock()
	lastGoodRaw = raw
	lastGoodAt = time.Now()
	lastErr = ""
	cacheMu.Unlock()

	return nil
}

/* =====================
   TAGO refresher (Stage 2 cache)
===================== */

func startTagoRefresher() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			cityCode, nodeID, err := getTagoCityNode()
			if err != nil {
				setTagoCache(nil, err)
				continue
			}
			m, err := fetchArrivalsTAGO(cityCode, nodeID)
			setTagoCache(m, err)
		}
	}()
}

/* =====================
   Server
===================== */

func main() {
	// ✅ .env를 자동으로 읽게(있으면 로드, 없으면 무시)
	loadDotEnvBestEffort()

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		cacheMu.RLock()
		ok := len(lastGoodRaw) > 0
		at := lastGoodAt
		le := lastErr

		n := 5
		if len(failLogs) < n {
			n = len(failLogs)
		}
		recent := make([]FailLog, n)
		if n > 0 {
			copy(recent, failLogs[len(failLogs)-n:])
		}
		cacheMu.RUnlock()

		_, tagoAt, tagoErr, tagoKeys := getTagoCacheCopy()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"cache_ready":     ok,
			"cache_at":        at.Format(time.RFC3339),
			"last_error":      le,
			"recent_fails":    recent,
			"tago_cache_at":   tagoAt.Format(time.RFC3339),
			"tago_last_error": tagoErr,
			"tago_last_keys":  tagoKeys,
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

		// BIT 기준: 단말은 503 처리 못할 수 있어 200 + notice로 통일
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rawState)
	})

	// screen.png 서빙 (BIT 단말은 이것만 GET)
	mux.HandleFunc("/display/screen.png", func(w http.ResponseWriter, r *http.Request) {
		screenPath := getenvDefault("SCREEN_PATH", "/data/screen.png")
		b, err := os.ReadFile(screenPath)
		if err != nil {
			http.Error(w, "screen not ready", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	// 시작할 때 1번 즉시 갱신(Directus 캐시)
	_ = refreshCacheOnce()

	// 30초마다 Directus 갱신
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_ = refreshCacheOnce()
		}
	}()

	// ✅ TAGO는 5초 주기 캐시
	startTagoRefresher()

	// ✅ state -> PNG 렌더 파이프라인 (screen.png 주기 갱신)
	startRenderRefresher()

	log.Println("go-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
