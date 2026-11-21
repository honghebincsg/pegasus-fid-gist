package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ghAPI        = "https://api.github.com"
	ghRepo       = "honghebincsg/pegasus-fid-gist"
	atisWorkflow = "423862105"
	gistRawBase  = "https://gist.githubusercontent.com/honghebincsg/0f820895dffee67ecf897420e1e430cb/raw/"
	runWaitLimit = 150 * time.Second
)

var (
	atisMu     sync.Mutex
	airportRe  = regexp.MustCompile(`^[A-Z]{4}$`)
	atisTimeRe = regexp.MustCompile(`\b(\d{4})Z\b`)
	httpClient = &http.Client{Timeout: 30 * time.Second}
	errNoATIS  = errors.New("no atis data")
)

func atisGistName(code string) string {
	return strings.ToUpper(code) + ".json"
}

func allowCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Cache-Control", "no-store")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func handleATIS(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	if code == "" && r.Method == http.MethodPost {
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		code = strings.ToUpper(strings.TrimSpace(req.Code))
	}
	if !airportRe.MatchString(code) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请输入 4 位 ICAO 机场代码"})
		return
	}
	atisMu.Lock()
	defer atisMu.Unlock()
	data, err := fetchATIS(code)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := saveATIS(data); err != nil {
		log.Printf("save ATIS: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func handleATISBatch(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	data, err := fetchBatchATIS()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

// fetchBatchATIS 从 Gist 读取批量缓存，并补全机场名称与跑道信息。
// Gist CDN 偶发 404/TLS 握手超时，失败时短暂重试。
func fetchBatchATIS() (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 1500 * time.Millisecond)
		}
		data, err := fetchBatchATISOnce()
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func fetchBatchATISOnce() (map[string]any, error) {
	url := fmt.Sprintf("%satis_all.json?t=%d", gistRawBase, time.Now().UnixNano())
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("读取缓存失败：%v", err)
	}
	// 手动刷新时必须跳过 CDN 缓存，取 GitHub 上的最新版本
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取缓存失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("缓存尚未生成，请等待首次批量抓取完成")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取缓存失败（HTTP %d）", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("缓存格式错误：%v", err)
	}
	if airports, ok := data["airports"].([]any); ok {
		for _, item := range airports {
			if block, ok := item.(map[string]any); ok {
				code, _ := block["airport"].(string)
				finalizeATIS(block, code)
			}
		}
	}
	return data, nil
}

func tokenPath() string {
	if p := os.Getenv("GH_DISPATCH_TOKEN_FILE"); p != "" {
		return p
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			base = dir
		} else {
			base = "."
		}
	}
	return filepath.Join(base, "anc-fid", "dispatch_token.txt")
}

func dispatchToken() string {
	if v := strings.TrimSpace(os.Getenv("GH_DISPATCH_TOKEN")); v != "" {
		return v
	}
	if data, err := os.ReadFile(tokenPath()); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	return embeddedDispatchToken
}

func ghRequest(method, url, token string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(req)
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05Z")
}

func extractATISTime(text string) string {
	if match := atisTimeRe.FindString(text); match != "" {
		return match
	}
	return ""
}

// fetchATISInfo 直连 atis.info（美国机场），失败返回 nil。
func fetchATISInfo(code string) map[string]any {
	url := "https://atis.info/api/" + code
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var items []struct {
		Airport string `json:"airport"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		DATIS   string `json:"datis"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil || len(items) == 0 {
		return nil
	}
	var combined map[string]any
	arrival := map[string]any{"updated_at": "", "text": ""}
	departure := map[string]any{"updated_at": "", "text": ""}
	for _, item := range items {
		if strings.TrimSpace(item.DATIS) == "" {
			continue
		}
		block := map[string]any{
			"code":       item.Code,
			"updated_at": extractATISTime(item.DATIS),
			"text":       item.DATIS,
		}
		switch strings.ToLower(item.Type) {
		case "combined":
			combined = block
		case "arrival":
			arrival = block
		case "departure":
			departure = block
		}
	}
	if combined == nil && arrival["text"] == "" && departure["text"] == "" {
		return nil
	}
	return finalizeATIS(map[string]any{
		"source_url": url,
		"airport":    code,
		"source":     "atis.info",
		"fetch_mode": "direct",
		"fetched_at": utcNow(),
		"combined":   combined,
		"arrival":    arrival,
		"departure":  departure,
	}, code)
}

var (
	scriptRe = regexp.MustCompile(`(?is)<script.*?</script>`)
	styleRe  = regexp.MustCompile(`(?is)<style.*?</style>`)
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

func cleanATISText(value string) string {
	value = spaceRe.ReplaceAllString(value, " ")
	if idx := strings.Index(value, "An unhandled error"); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func stripHTML(source string) string {
	source = scriptRe.ReplaceAllString(source, " ")
	source = styleRe.ReplaceAllString(source, " ")
	source = tagRe.ReplaceAllString(source, " ")
	source = html.UnescapeString(source)
	source = strings.ReplaceAll(source, "\r", "")
	return strings.TrimSpace(source)
}

// fetchATISGuru 本机直连 atis.guru 页面。
// 返回值 reachable 表示页面是否成功取到（用于区分“无数据”和“网络失败”）。
func fetchATISGuru(code string) (map[string]any, bool) {
	url := "https://atis.guru/atis/" + code
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, false
	}
	one := spaceRe.ReplaceAllString(stripHTML(string(body)), " ")
	grab := func(pattern string) string {
		match := regexp.MustCompile("(?is)" + pattern).FindStringSubmatch(one)
		if len(match) > 1 {
			return cleanATISText(match[1])
		}
		return ""
	}
	prefix := regexp.QuoteMeta(code)
	arrival := map[string]any{
		"updated_at": grab(`Arrival ATIS ([\d-]+ [\d:]+) UTC`),
		"text":       grab(prefix + ` ARR ATIS (.*?) Departure ATIS`),
	}
	departure := map[string]any{
		"updated_at": grab(`Departure ATIS ([\d-]+ [\d:]+) UTC`),
		"text":       grab(prefix + ` DEP ATIS (.*?) METAR`),
	}
	if arrival["text"] == "" && departure["text"] == "" {
		return nil, true
	}
	data := map[string]any{
		"source_url":      url,
		"airport":         code,
		"airport_name_en": grab(`Live digital ATIS for (.*?) Arrival ATIS`),
		"source":          "atis.guru",
		"fetch_mode":      "direct",
		"fetched_at":      utcNow(),
		"combined":        nil,
		"arrival":         arrival,
		"departure":       departure,
	}
	return finalizeATIS(data, code), true
}

func atisFromGistBody(resp *http.Response, code string) (map[string]any, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		return nil, fmt.Errorf("invalid json")
	}
	if airport, _ := data["airport"].(string); !strings.EqualFold(airport, code) {
		return nil, fmt.Errorf("airport mismatch")
	}
	if ok, exists := data["ok"].(bool); exists && !ok {
		message, _ := data["error"].(string)
		if message == "" {
			message = "该机场暂无 ATIS 数据"
		}
		return nil, fmt.Errorf("%w: %s", errNoATIS, message)
	}
	return data, nil
}


func triggerATISWorkflow(code string) {
	token := dispatchToken()
	if token == "" {
		return
	}
	resp, err := ghRequest("POST",
		fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", ghAPI, ghRepo, atisWorkflow),
		token, map[string]any{"ref": "main", "inputs": map[string]string{"airport": code}})
	if err != nil {
		log.Printf("ATIS gist sync failed: %v", err)
		return
	}
	resp.Body.Close()
}

func fetchATIS(code string) (map[string]any, error) {
	if data := fetchATISInfo(code); data != nil {
		go triggerATISWorkflow(code)
		return data, nil
	}
	if data, reachable := fetchATISGuru(code); data != nil {
		go triggerATISWorkflow(code)
		return data, nil
	} else if reachable {
		return nil, fmt.Errorf("该机场在 atis.guru 上暂无 ATIS 数据，请确认四字码是否正确")
	}
	token := dispatchToken()
	if token == "" {
		return nil, fmt.Errorf("未配置 GitHub token，请把带 Actions 写权限的 token 保存到：%s", tokenPath())
	}
	since := time.Now().UTC()
	resp, err := ghRequest("POST",
		fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", ghAPI, ghRepo, atisWorkflow),
		token, map[string]any{"ref": "main", "inputs": map[string]string{"airport": code}})
	if err != nil {
		return nil, fmt.Errorf("触发抓取失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("触发抓取失败（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := waitForATISRun(token, since); err != nil {
		return nil, err
	}
	data, err := readATISGist(code)
	if errors.Is(err, errNoATIS) {
		return nil, fmt.Errorf("该机场暂无 ATIS 数据")
	}
	if err == nil {
		data["fetch_mode"] = "gist"
		data = finalizeATIS(data, code)
	}
	return data, err
}

func waitForATISRun(token string, since time.Time) error {
	deadline := time.Now().Add(runWaitLimit)
	for time.Now().Before(deadline) {
		resp, err := ghRequest("GET",
			fmt.Sprintf("%s/repos/%s/actions/runs?per_page=10", ghAPI, ghRepo), token, nil)
		if err == nil {
			var payload struct {
				WorkflowRuns []struct {
					Path       string    `json:"path"`
					Status     string    `json:"status"`
					Conclusion string    `json:"conclusion"`
					CreatedAt  time.Time `json:"created_at"`
				} `json:"workflow_runs"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			if decodeErr == nil {
				for _, run := range payload.WorkflowRuns {
					if run.Path != ".github/workflows/fetch-atis.yml" {
						continue
					}
					if run.CreatedAt.Before(since.Add(-30 * time.Second)) {
						continue
					}
					if run.Status == "completed" {
						if run.Conclusion != "success" {
							return fmt.Errorf("Actions 运行失败：%s", run.Conclusion)
						}
						return nil
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("等待抓取超时，请稍后重试")
}

func readATISGist(code string) (map[string]any, error) {
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		url := fmt.Sprintf("%s%s?t=%d", gistRawBase, atisGistName(code), time.Now().Unix())
		resp, err := httpClient.Get(url)
		if err != nil {
			lastErr = err
		} else {
			data, err := atisFromGistBody(resp, code)
			if err == nil {
				return data, nil
			}
			lastErr = err
			if errors.Is(err, errNoATIS) {
				return nil, err
			}
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("抓取完成但未读到 %s 的数据，请稍后重试", code)
}
