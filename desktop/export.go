package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type csvExportRequest struct {
	Filename string `json:"filename"`
	CSV      string `json:"csv"`
}

func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// handleExportCSV 把页面生成的 CSV 直接保存到系统下载目录，避免 WebView2 的
// “允许多次下载”权限提示。
func handleExportCSV(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var req csvExportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	name := strings.TrimSpace(req.Filename)
	if base := filepath.Base(name); base != "" && base != "." {
		name = base
	} else {
		name = "export.csv"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".csv") {
		name += ".csv"
	}
	dir, err := downloadsDir()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	path := filepath.Join(dir, name)
	if _, statErr := os.Stat(path); statErr == nil {
		stem := strings.TrimSuffix(name, ".csv")
		path = filepath.Join(dir, fmt.Sprintf("%s-%s.csv", stem, time.Now().Format("150405")))
	}
	if err := os.WriteFile(path, []byte(req.CSV), 0o644); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}
