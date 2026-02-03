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
	NodeID    string `json:"node_id"`
	Enabled   bool   `json:"enabled"`
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
	At        time.Time `json:"at"`
	DisplayID int       `json:"display_id,omitempty"`
	Where     string    `json:"where"`
	Reason    string    `json:"reason"`
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
	cacheMu       sync.RWMutex
	displayCaches = make(map[int]*displayCache)

	// 최근 50개 실패 로그
	failLogs    []FailLog
	failLogsMax = 50
)

type displayCache struct {
	Raw           []byte
	Config        DisplayConfig
	LastGoodAt    time.Time
	LastErr       string
	LastErrShort  string
	LastOkStateAt time.Time
	LastOkPNGAt   time.Time
	LastStateHash string
	NextRenderAt  time.Time
	RenderRunning bool
}

func addFailLog(displayID int, where string, err error) {
	if err == nil {
		return
	}
	fl := FailLog{
		At:        time.Now(),
		DisplayID: displayID,
		Where:     where,
		Reason:    err.Error(),
	}

	cacheMu.Lock()
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
	tagoMu     sync.RWMutex
	tagoCaches = make(map[int]*tagoCache)
)

type tagoCache struct {
	LastMap  map[string]ETASnapshot
	LastAt   time.Time
	LastErr  string
	LastKeys []string
}

func setTagoCache(displayID int, m map[string]ETASnapshot, err error) {
	tagoMu.Lock()
	defer tagoMu.Unlock()

	cache := tagoCaches[displayID]
	if cache == nil {
		cache = &tagoCache{}
		tagoCaches[displayID] = cache
	}

	if err != nil {
		cache.LastErr = err.Error()
		// 실패해도 기존 캐시는 유지 (죽지 말고 흐리게)
		return
	}

	cache.LastMap = m
	cache.LastAt = time.Now()
	cache.LastErr = ""

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cache.LastKeys = keys
}

func getTagoCacheCopy(displayID int) (map[string]ETASnapshot, time.Time, string, []string) {
	tagoMu.RLock()
	defer tagoMu.RUnlock()

	cache := tagoCaches[displayID]
	if cache == nil || cache.LastMap == nil {
		return map[string]ETASnapshot{}, time.Time{}, "", nil
	}
	out := make(map[string]ETASnapshot, len(cache.LastMap))
	for k, v := range cache.LastMap {
		out[k] = v
	}
	keys := append([]string(nil), cache.LastKeys...)
	return out, cache.LastAt, cache.LastErr, keys
}

/* =====================
   Render Pipeline (state -> PNG)
===================== */

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

func sanitizeNotice(input string) string {
	const maxLen = 24
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "정상"
	}
	var b strings.Builder
	for _, r := range trimmed {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 32 || r == 127 {
			continue
		}
		if r > 0x1F000 {
			b.WriteRune('□')
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return "정상"
	}
	return clampText(cleaned, maxLen)
}

func summarizeError(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	compact := strings.Join(strings.Fields(trimmed), " ")
	return clampText(compact, 80)
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

func getCachedDisplaySizeFallback(displayID int) (int, int) {
	cacheMu.RLock()
	cache := displayCaches[displayID]
	cacheMu.RUnlock()

	// 기본값
	w, h := 320, 160
	if cache == nil {
		return w, h
	}

	if cache.Config.Display.Width > 0 {
		w = cache.Config.Display.Width
	}
	if cache.Config.Display.Height > 0 {
		h = cache.Config.Display.Height
	}
	return w, h
}

func screenPathForDisplay(displayID int) string {
	if displayID == 1 {
		if legacy := strings.TrimSpace(os.Getenv("SCREEN_PATH")); legacy != "" {
			return legacy
		}
	}
	pattern := getenvDefault("SCREEN_PATH_PATTERN", "/data/screen_%d.png")
	return fmt.Sprintf(pattern, displayID)
}

func startRenderRefresher() {
	// renderer-service의 POST /render 를 직접 가리키도록 통일
	// 예: http://renderer:3000/render
	rendererURL := getenvDefault("RENDERER_URL", "http://renderer:3000/render")

	// env로 주기 강제 가능
	overrideSec := getenvIntDefault("RENDER_INTERVAL_SEC", 0)
	concurrency := getenvIntDefault("RENDER_CONCURRENCY", 1)
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			displayIDs := getCachedDisplayIDs()
			for _, displayID := range displayIDs {
				if !isDisplayEnabled(displayID) {
					continue
				}
				if !markRenderRunningIfDue(displayID) {
					continue
				}
				go func(id int) {
					sem <- struct{}{}
					defer func() {
						<-sem
						markRenderDone(id)
					}()
					renderOneDisplay(rendererURL, overrideSec, id)
				}(displayID)
			}
		}
	}()
}

func markRenderRunningIfDue(displayID int) bool {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache := displayCaches[displayID]
	if cache == nil {
		return false
	}
	if cache.RenderRunning {
		return false
	}
	if !cache.NextRenderAt.IsZero() && time.Now().Before(cache.NextRenderAt) {
		return false
	}
	cache.RenderRunning = true
	return true
}

func markRenderDone(displayID int) {
	cacheMu.Lock()
	if cache := displayCaches[displayID]; cache != nil {
		cache.RenderRunning = false
	}
	cacheMu.Unlock()
}

func renderOneDisplay(rendererURL string, overrideSec int, displayID int) {
	cacheMu.RLock()
	hasCache := displayCaches[displayID] != nil && len(displayCaches[displayID].Raw) > 0
	cacheMu.RUnlock()
	if !hasCache {
		_ = refreshCacheForDisplay(displayID)
	}

	st, _, err := buildStateFromCachedConfig(displayID)
	if err != nil {
		addFailLog(displayID, "render_state_build", err)
		setNextRender(displayID, overrideSec, st.RefreshSec)
		return
	}

	intervalSec := nextIntervalSec(overrideSec, st.RefreshSec)
	setNextRender(displayID, intervalSec, 0)

	w, h := getCachedDisplaySizeFallback(displayID)

	rawForHash, err := json.Marshal(st)
	if err == nil {
		hv := sha256Hex(rawForHash)
		if isSameStateHash(displayID, hv) {
			return
		}
		setStateHash(displayID, hv)
	}

	png, err := callRendererPNG(rendererURL, w, h, st)
	if err != nil {
		addFailLog(displayID, "renderer_call", err)
		return
	}
	screenPath := screenPathForDisplay(displayID)
	if err := atomicWriteFile(screenPath, png); err != nil {
		addFailLog(displayID, "screen_write", err)
		return
	}

	cacheMu.Lock()
	if cache := displayCaches[displayID]; cache != nil {
		cache.LastOkPNGAt = time.Now()
	}
	cacheMu.Unlock()
}

func setNextRender(displayID int, intervalSec int, refreshSec int) {
	interval := nextIntervalSec(intervalSec, refreshSec)
	cacheMu.Lock()
	if cache := displayCaches[displayID]; cache != nil {
		cache.NextRenderAt = time.Now().Add(time.Duration(interval) * time.Second)
	}
	cacheMu.Unlock()
}

func nextIntervalSec(overrideSec int, refreshSec int) int {
	intervalSec := 5
	if overrideSec > 0 {
		intervalSec = overrideSec
	} else if refreshSec > 0 {
		intervalSec = refreshSec
	}
	if intervalSec < 1 {
		intervalSec = 1
	}
	return intervalSec
}

func isSameStateHash(displayID int, hash string) bool {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	cache := displayCaches[displayID]
	if cache == nil {
		return false
	}
	return cache.LastStateHash == hash
}

func setStateHash(displayID int, hash string) {
	cacheMu.Lock()
	if cache := displayCaches[displayID]; cache != nil {
		cache.LastStateHash = hash
	}
	cacheMu.Unlock()
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

func getTagoCityCode() (int, error) {
	cityStr := strings.TrimSpace(os.Getenv("TAGO_CITY_CODE"))

	if cityStr == "" {
		return 0, fmt.Errorf("TAGO_CITY_CODE is empty")
	}
	cityCode, err := strconv.Atoi(cityStr)
	if err != nil || cityCode <= 0 {
		return 0, fmt.Errorf("TAGO_CITY_CODE invalid: %q", cityStr)
	}
	return cityCode, nil
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
			if _, ok := out[k]; !ok {
				out[k] = ETASnapshot{ETASec: nil, Ended: false}
			}
			continue
		}

		tmp := sec
		if existing, ok := out[k]; ok && existing.ETASec != nil {
			if *existing.ETASec <= sec {
				continue
			}
		}
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
	cache := displayCaches[displayID]
	cacheMu.RUnlock()

	if cache == nil || len(cache.Raw) == 0 {
		st := StateResponse{
			Theme:      "default",
			RefreshSec: 30,
			Notice:     "cache not ready",
			Routes:     []StateRoute{},
		}
		return st, mustMarshalState(st), fmt.Errorf("cache not ready")
	}

	// TAGO 캐시 사용 (요청마다 fetch 금지)
	arrMap, _, tagoErr, _ := getTagoCacheCopy(displayID)

	noticeParts := make([]string, 0, 2)
	if tagoErr != "" {
		noticeParts = append(noticeParts, "실시간 조회 실패")
	}
	notice := sanitizeNotice(strings.Join(noticeParts, " | "))

	out := make([]StateRoute, 0, len(cache.Config.Routes))

	for _, r := range cache.Config.Routes {
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

	out = limitRoutes(out, cache.Config.Settings.MaxRoutes)

	state := StateResponse{
		Theme:      cache.Config.Settings.Theme,
		RefreshSec: cache.Config.Settings.RefreshSec,
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

	cacheMu.Lock()
	if cache := displayCaches[displayID]; cache != nil {
		cache.LastOkStateAt = time.Now()
	}
	cacheMu.Unlock()

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
			NodeID:    "DUMMY",
			Enabled:   true,
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
	NodeID    string `json:"node_id"`
	Enabled   bool   `json:"enabled"`
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
			NodeID:    disp.NodeID,
			Enabled:   disp.Enabled,
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

func fetchEnabledDisplays() ([]directusDisplayItem, error) {
	if isDummyMode() {
		return []directusDisplayItem{{
			ID:        1,
			DisplayID: 1,
			Name:      "Test Display",
			Width:     320,
			Height:    160,
			NodeID:    "DUMMY",
			Enabled:   true,
		}}, nil
	}

	base := getDirectusBaseURL()
	if base == "" {
		return nil, fmt.Errorf("DIRECTUS_URL is empty")
	}

	var resp directusListResp[directusDisplayItem]
	url := fmt.Sprintf("%s/items/displays?filter[enabled][_eq]=true&limit=200", base)
	if err := fetchJSON(url, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

/* =====================
   Cache Refresh (Directus)
===================== */

func refreshCacheForDisplay(displayID int) error {
	cfg, raw, err := fetchConfigFromDirectus(displayID)
	cacheMu.Lock()
	defer cacheMu.Unlock()

	cache := displayCaches[displayID]
	if cache == nil {
		cache = &displayCache{}
		displayCaches[displayID] = cache
	}

	if err != nil {
		cache.LastErr = err.Error()
		cache.LastErrShort = summarizeError(err.Error())
		addFailLog(displayID, "directus_fetch", err)
		return err
	}

	cache.Raw = raw
	cache.Config = cfg
	cache.LastGoodAt = time.Now()
	cache.LastErr = ""
	cache.LastErrShort = ""
	return nil
}

func refreshCacheAll() error {
	displays, err := fetchEnabledDisplays()
	if err != nil {
		addFailLog(0, "directus_display_list", err)
		return err
	}

	displayIDs := make(map[int]struct{}, len(displays))
	for _, d := range displays {
		displayIDs[d.DisplayID] = struct{}{}
		_ = refreshCacheForDisplay(d.DisplayID)
	}

	cacheMu.Lock()
	for id := range displayCaches {
		if _, ok := displayIDs[id]; !ok {
			delete(displayCaches, id)
			tagoMu.Lock()
			delete(tagoCaches, id)
			tagoMu.Unlock()
		}
	}
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
			cityCode, err := getTagoCityCode()
			if err != nil {
				displayIDs := getCachedDisplayIDs()
				for _, displayID := range displayIDs {
					setTagoCache(displayID, nil, err)
				}
				continue
			}

			displayIDs := getCachedDisplayIDs()
			for _, displayID := range displayIDs {
				cacheMu.RLock()
				cache := displayCaches[displayID]
				cacheMu.RUnlock()
				if cache == nil || strings.TrimSpace(cache.Config.Display.NodeID) == "" {
					continue
				}
				m, err := fetchArrivalsTAGO(cityCode, cache.Config.Display.NodeID)
				setTagoCache(displayID, m, err)
			}
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
		n := 5
		if len(failLogs) < n {
			n = len(failLogs)
		}
		recent := make([]FailLog, n)
		if n > 0 {
			copy(recent, failLogs[len(failLogs)-n:])
		}
		displayStatus := make(map[int]map[string]any, len(displayCaches))
		for id, cache := range displayCaches {
			tagoAt, tagoErr, tagoKeys := getTagoMeta(id)
			displayStatus[id] = map[string]any{
				"cache_ready":        len(cache.Raw) > 0,
				"cache_at":           cache.LastGoodAt.Format(time.RFC3339),
				"last_error":         cache.LastErr,
				"last_error_summary": cache.LastErrShort,
				"last_ok_state_at":   cache.LastOkStateAt.Format(time.RFC3339),
				"last_ok_png_at":     cache.LastOkPNGAt.Format(time.RFC3339),
				"tago_cache_at":      tagoAt.Format(time.RFC3339),
				"tago_last_error":    tagoErr,
				"tago_last_keys":     tagoKeys,
				"node_id":            cache.Config.Display.NodeID,
			}
		}
		cacheMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"recent_fails": recent,
			"displays":     displayStatus,
		})
	})

	mux.HandleFunc("/v1/display/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/display/")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		displayID, err := strconv.Atoi(parts[0])
		if err != nil || displayID <= 0 {
			http.Error(w, "invalid display id", http.StatusBadRequest)
			return
		}
		switch parts[1] {
		case "config":
			serveDisplayConfig(w, r, displayID)
		case "state":
			serveDisplayState(w, r, displayID)
		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc("/v1/display/1/config", func(w http.ResponseWriter, r *http.Request) {
		serveDisplayConfig(w, r, 1)
	})

	mux.HandleFunc("/v1/display/1/state", func(w http.ResponseWriter, r *http.Request) {
		serveDisplayState(w, r, 1)
	})

	// screen.png 서빙 (BIT 단말은 이것만 GET)
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

	// 시작할 때 display 목록 즉시 갱신
	_ = refreshCacheAll()

	// 30초마다 Directus 갱신
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_ = refreshCacheAll()
		}
	}()

	// ✅ TAGO는 5초 주기 캐시
	startTagoRefresher()

	// ✅ state -> PNG 렌더 파이프라인 (screen.png 주기 갱신)
	startRenderRefresher()

	log.Println("go-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func serveDisplayConfig(w http.ResponseWriter, r *http.Request, displayID int) {
	cacheMu.RLock()
	cache := displayCaches[displayID]
	cacheMu.RUnlock()

	if cache != nil && len(cache.Raw) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cache.Raw)
		return
	}

	if err := refreshCacheForDisplay(displayID); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	cacheMu.RLock()
	cache = displayCaches[displayID]
	cacheMu.RUnlock()

	if cache == nil || len(cache.Raw) == 0 {
		http.Error(w, "cache not ready", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cache.Raw)
}

func serveDisplayState(w http.ResponseWriter, r *http.Request, displayID int) {
	cacheMu.RLock()
	hasCache := displayCaches[displayID] != nil && len(displayCaches[displayID].Raw) > 0
	cacheMu.RUnlock()

	if !hasCache {
		_ = refreshCacheForDisplay(displayID)
	}

	_, rawState, err := buildStateFromCachedConfig(displayID)
	if err != nil {
		addFailLog(displayID, "state_build", err)
	}

	// BIT 기준: 단말은 503 처리 못할 수 있어 200 + notice로 통일
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rawState)
}

func serveScreenPNG(w http.ResponseWriter, r *http.Request, displayID int) {
	screenPath := screenPathForDisplay(displayID)
	b, err := os.ReadFile(screenPath)
	if err != nil {
		http.Error(w, "screen not ready", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	sum := sha256.Sum256(b)
	w.Header().Set("ETag", fmt.Sprintf("\"%x\"", sum[:]))
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func getCachedDisplayIDs() []int {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	ids := make([]int, 0, len(displayCaches))
	for id := range displayCaches {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func isDisplayEnabled(displayID int) bool {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	cache := displayCaches[displayID]
	if cache == nil {
		return false
	}
	return cache.Config.Display.Enabled
}

func getTagoMeta(displayID int) (time.Time, string, []string) {
	tagoMu.RLock()
	defer tagoMu.RUnlock()
	cache := tagoCaches[displayID]
	if cache == nil {
		return time.Time{}, "", nil
	}
	keys := append([]string(nil), cache.LastKeys...)
	return cache.LastAt, cache.LastErr, keys
}
