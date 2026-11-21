package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	historyRepo     = "honghebincsg/pegasus-fid-gist"
	historyIndexURL = "atis-history/index.json"
	historyCacheTTL = 5 * time.Minute
)

type atisHistoryRow struct {
	Airport      string   `json:"airport"`
	ArrivalAt    string   `json:"arrival_at"`
	DepartureAt  string   `json:"departure_at"`
	Time         string   `json:"time"`
	Info         string   `json:"info"`
	Landing      []string `json:"landing"`
	DepartRunway []string `json:"departure"`
}

type historyMonthCache struct {
	rows []atisHistoryRow
	at   time.Time
}

var (
	histMu      sync.Mutex
	histIndex   map[string][]string
	histIndexAt time.Time
	histMonths  = map[string]historyMonthCache{}
)

func readHistoryFile(path string) (string, error) {
	token := dispatchToken()
	req, err := http.NewRequest("GET",
		fmt.Sprintf("%s/repos/%s/contents/%s?ref=main", ghAPI, historyRepo, path), nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func loadHistoryIndex() (map[string][]string, error) {
	if histIndex != nil && time.Since(histIndexAt) < historyCacheTTL {
		return histIndex, nil
	}
	text, err := readHistoryFile(historyIndexURL)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Airports map[string][]string `json:"airports"`
	}
	if text != "" {
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			return nil, err
		}
	}
	histIndex = payload.Airports
	histIndexAt = time.Now()
	return histIndex, nil
}

func splitHistoryRunways(field string) []string {
	field = strings.TrimSpace(field)
	if field == "" || field == "-" {
		return nil
	}
	return strings.Fields(field)
}

func parseHistoryCSV(text string) []atisHistoryRow {
	rows := make([]atisHistoryRow, 0, 64)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		row := atisHistoryRow{
			Airport:      parts[0],
			ArrivalAt:    strings.TrimSpace(parts[1]),
			DepartureAt:  strings.TrimSpace(parts[2]),
			Info:         strings.TrimSpace(parts[3]),
			Landing:      splitHistoryRunways(parts[4]),
			DepartRunway: splitHistoryRunways(parts[5]),
		}
		if row.ArrivalAt == "-" {
			row.ArrivalAt = ""
		}
		if row.DepartureAt == "-" {
			row.DepartureAt = ""
		}
		if row.Info == "-" {
			row.Info = ""
		}
		row.Time = row.ArrivalAt
		if row.DepartureAt > row.Time {
			row.Time = row.DepartureAt
		}
		rows = append(rows, row)
	}
	return rows
}

func loadATISHistory(code string, monthsWanted int) ([]atisHistoryRow, []string, error) {
	histMu.Lock()
	defer histMu.Unlock()
	index, err := loadHistoryIndex()
	if err != nil {
		return nil, nil, fmt.Errorf("历史索引读取失败：%v", err)
	}
	all := index[code]
	if len(all) > monthsWanted {
		all = all[len(all)-monthsWanted:]
	}
	rows := make([]atisHistoryRow, 0, 128)
	for _, month := range all {
		cacheKey := code + "/" + month
		if cached, ok := histMonths[cacheKey]; ok && time.Since(cached.at) < historyCacheTTL {
			rows = append(rows, cached.rows...)
			continue
		}
		text, err := readHistoryFile(fmt.Sprintf("atis-history/%s/%s.csv", code, month))
		if err != nil {
			return nil, nil, fmt.Errorf("读取 %s 失败：%v", month, err)
		}
		monthRows := parseHistoryCSV(text)
		histMonths[cacheKey] = historyMonthCache{rows: monthRows, at: time.Now()}
		rows = append(rows, monthRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time > rows[j].Time })
	return rows, all, nil
}

func handleATISHistory(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("airport")))
	if !airportRe.MatchString(code) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请输入 4 位 ICAO 机场代码"})
		return
	}
	months := 6
	if raw := r.URL.Query().Get("months"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			months = value
		}
	}
	if months < 1 {
		months = 1
	}
	if months > 24 {
		months = 24
	}
	rows, usedMonths, err := loadATISHistory(code, months)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
		"airport": code,
		"months":  usedMonths,
		"count":   len(rows),
		"rows":    rows,
	}})
}
