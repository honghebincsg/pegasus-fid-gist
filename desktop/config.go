package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// 用户设置保存在 exe 同目录的 config.json，重启后保留。

type appSettings struct {
	Theme     string `json:"theme"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

var settingsMu sync.Mutex

func settingsPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

func loadSettings() appSettings {
	s := appSettings{Theme: "dark"}
	if data, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if s.Theme != "light" {
		s.Theme = "dark"
	}
	return s
}

func saveSettings(s appSettings) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath(), data, 0o644)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": loadSettings()})
	case http.MethodPost:
		var req appSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		if req.Theme != "light" && req.Theme != "dark" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "invalid theme"})
			return
		}
		cur := loadSettings()
		// 旧请求（时间戳更早）不覆盖更新的选择，避免网络延迟导致闪烁/回退
		if req.UpdatedAt > 0 && req.UpdatedAt < cur.UpdatedAt {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stale": true, "data": cur})
			return
		}
		next := cur
		next.Theme = req.Theme
		if req.UpdatedAt > 0 {
			next.UpdatedAt = req.UpdatedAt
		}
		if err := saveSettings(next); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
	}
}
