package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 距上次观察超过该时长时只更新基线，不告警（避免关机很久后重开炸出一堆旧告警）。
const gateAlertWindow = 6 * time.Hour

type gateAlert struct {
	ID           int64
	FlightKey    string
	Flight       string
	LSODate      string
	Registration string
	ACType       string
	OldGate      string
	NewGate      string
	ObservedAt   string
}

var (
	pendingHistory       atomic.Bool
	pendingHistoryFlight atomic.Value
)

func initGateSchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS gate_alerts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			flight_key TEXT NOT NULL,
			flight_number TEXT,
			lsodate TEXT,
			registration TEXT,
			ac_type TEXT,
			old_gate TEXT NOT NULL,
			new_gate TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			acked_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gate_alerts_observed ON gate_alerts(observed_at)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// lastObservationRecent 判断上次观察时间是否在告警窗口内。
func lastObservationRecent(lastSeen string) bool {
	value := strings.TrimSpace(lastSeen)
	if value == "" {
		return false
	}
	observed, err := time.Parse("2006-01-02 15:04:05Z", value)
	if err != nil {
		return false
	}
	gap := time.Since(observed)
	return gap <= gateAlertWindow && gap >= -10*time.Minute
}

func recordGateAlert(tx *sql.Tx, key string, flight map[string]any, oldGate, newGate, observedAt string) (gateAlert, error) {
	alert := gateAlert{
		FlightKey:    key,
		Flight:       strings.TrimSpace(valueString(flight["FlightNumber"])),
		LSODate:      strings.TrimSpace(valueString(flight["LSODate"])),
		Registration: strings.TrimSpace(valueString(flight["RegistrationNumber"])),
		ACType:       strings.TrimSpace(valueString(flight["ACType"])),
		OldGate:      oldGate,
		NewGate:      newGate,
		ObservedAt:   observedAt,
	}
	result, err := tx.Exec(`
		INSERT INTO gate_alerts (flight_key, flight_number, lsodate, registration, ac_type,
			old_gate, new_gate, observed_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		alert.FlightKey, alert.Flight, alert.LSODate, alert.Registration, alert.ACType,
		alert.OldGate, alert.NewGate, alert.ObservedAt, observedAt)
	if err != nil {
		return alert, err
	}
	alert.ID, _ = result.LastInsertId()
	return alert, nil
}

func setPendingGateHistory(flight string) {
	pendingHistoryFlight.Store(flight)
	pendingHistory.Store(true)
}

func handleGateHistoryPending(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	flight, _ := pendingHistoryFlight.Load().(string)
	pending := false
	if pendingHistory.CompareAndSwap(true, false) {
		pending = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": pending, "flight": flight})
}

// ---------- 停机位历史（仓库分片） ----------

type fidHistoryRow struct {
	Date         string `json:"date"`
	Flight       string `json:"flight"`
	Registration string `json:"registration"`
	ACType       string `json:"ac_type"`
	Gate         string `json:"gate"`
	STA          string `json:"sta"`
	ETA          string `json:"eta"`
	ATA          string `json:"ata"`
	STD          string `json:"std"`
	ETD          string `json:"etd"`
	ATD          string `json:"atd"`
	ObservedAt   string `json:"observed_at"`
	Alerted      bool   `json:"alerted"`
}

type fidHistoryCache struct {
	rows []fidHistoryRow
	at   time.Time
}

var (
	gateHistMu       sync.Mutex
	gateHistMonths   = map[string]fidHistoryCache{}
	gateAlertMarks   map[string]bool
	gateAlertMarksAt time.Time
)

func parseFIDHistoryCSV(text string) []fidHistoryRow {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil || len(records) < 2 {
		return nil
	}
	index := map[string]int{}
	for i, name := range records[0] {
		index[strings.TrimSpace(name)] = i
	}
	get := func(record []string, name string) string {
		if pos, ok := index[name]; ok && pos < len(record) {
			return strings.TrimSpace(record[pos])
		}
		return ""
	}
	rows := make([]fidHistoryRow, 0, len(records)-1)
	for _, record := range records[1:] {
		row := fidHistoryRow{
			Date:         get(record, "date"),
			Flight:       get(record, "flight"),
			Registration: get(record, "registration"),
			ACType:       get(record, "ac_type"),
			Gate:         get(record, "gate"),
			STA:          get(record, "sta"),
			ETA:          get(record, "eta"),
			ATA:          get(record, "ata"),
			STD:          get(record, "std"),
			ETD:          get(record, "etd"),
			ATD:          get(record, "atd"),
			ObservedAt:   get(record, "observed_at"),
		}
		if row.Flight == "" || row.Date == "" || row.Gate == "" {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

func loadGateAlertMarks() map[string]bool {
	if gateAlertMarks != nil && time.Since(gateAlertMarksAt) < historyCacheTTL {
		return gateAlertMarks
	}
	marks := map[string]bool{}
	if storeDB != nil {
		rows, err := storeDB.Query(`SELECT flight_number, lsodate, new_gate FROM gate_alerts ORDER BY id DESC LIMIT 2000`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var flight, date, gate sql.NullString
				if rows.Scan(&flight, &date, &gate) == nil && flight.String != "" {
					marks[flight.String+"|"+date.String+"|"+gate.String] = true
				}
			}
		}
	}
	gateAlertMarks = marks
	gateAlertMarksAt = time.Now()
	return marks
}

func loadFIDHistory(monthsWanted int) ([]fidHistoryRow, []string, error) {
	gateHistMu.Lock()
	defer gateHistMu.Unlock()
	now := time.Now().UTC()
	months := make([]string, 0, monthsWanted)
	for i := monthsWanted - 1; i >= 0; i-- {
		months = append(months, now.AddDate(0, -i, 0).Format("2006-01"))
	}
	rows := make([]fidHistoryRow, 0, 256)
	for _, month := range months {
		if cached, ok := gateHistMonths[month]; ok && time.Since(cached.at) < historyCacheTTL {
			rows = append(rows, cached.rows...)
			continue
		}
		text, err := readHistoryFile(fmt.Sprintf("fid-history/%s.csv", month))
		if err != nil {
			return nil, nil, fmt.Errorf("读取 %s 失败：%v", month, err)
		}
		monthRows := parseFIDHistoryCSV(text)
		gateHistMonths[month] = fidHistoryCache{rows: monthRows, at: time.Now()}
		rows = append(rows, monthRows...)
	}
	marks := loadGateAlertMarks()
	for i := range rows {
		rows[i].Alerted = marks[rows[i].Flight+"|"+rows[i].Date+"|"+rows[i].Gate]
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ObservedAt > rows[j].ObservedAt })
	return rows, months, nil
}

func handleFIDHistory(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	months := 3
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
	rows, usedMonths, err := loadFIDHistory(months)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	keyword := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("flight")))
	if keyword != "" {
		filtered := rows[:0]
		for _, row := range rows {
			haystack := strings.ToUpper(row.Flight + " " + row.Registration + " " + row.Gate)
			if strings.Contains(haystack, keyword) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
		"months": usedMonths,
		"count":  len(rows),
		"rows":   rows,
	}})
}
