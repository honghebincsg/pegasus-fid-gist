package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

var storeDB *sql.DB

func storePath() string {
	if path := strings.TrimSpace(os.Getenv("ANC_FID_DB")); path != "" {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "anc-fid.db")
	}
	return "anc-fid.db"
}

func openStore() {
	path := storePath()
	db, err := sql.Open("sqlite", path)
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			log.Printf("sqlite open failed: %v", err)
			return
		}
		fallback := filepath.Join(base, "anc-fid", "anc-fid.db")
		if mkErr := os.MkdirAll(filepath.Dir(fallback), 0o755); mkErr != nil {
			log.Printf("sqlite dir failed: %v", mkErr)
			return
		}
		db, err = sql.Open("sqlite", fallback)
		if err == nil {
			err = db.Ping()
		}
		if err != nil {
			log.Printf("sqlite open failed: %v", err)
			return
		}
		path = fallback
	}
	db.SetMaxOpenConns(1)
	if err := initSchema(db); err != nil {
		log.Printf("sqlite init failed: %v", err)
		db.Close()
		return
	}
	storeDB = db
	log.Printf("sqlite store: %s", path)
}

func initSchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS fid_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			fetched_at TEXT NOT NULL UNIQUE,
			flight_count INTEGER NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fid_flights (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_id INTEGER NOT NULL,
			recno TEXT,
			flight_number TEXT,
			registration TEXT,
			ac_type TEXT,
			gate TEXT,
			from_station TEXT,
			to_station TEXT,
			sta TEXT, eta TEXT, ata TEXT,
			std TEXT, etd TEXT, atd TEXT,
			lsodate TEXT,
			remarks TEXT,
			aog INTEGER,
			long_turn INTEGER,
			international INTEGER,
			UNIQUE(snapshot_id, recno)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fid_flights_gate ON fid_flights(gate)`,
		`CREATE INDEX IF NOT EXISTS idx_fid_flights_flight ON fid_flights(flight_number)`,
		`CREATE TABLE IF NOT EXISTS atis_queries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			airport TEXT NOT NULL,
			source TEXT,
			fetch_mode TEXT,
			atis_code TEXT,
			updated_at TEXT,
			fetched_at TEXT,
			query_at TEXT NOT NULL,
			text TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_atis_airport ON atis_queries(airport, query_at)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func valueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(typed)
	}
}

func flagInt(value any) int {
	if typed, ok := value.(bool); ok {
		if typed {
			return 1
		}
		return 0
	}
	text := valueString(value)
	if text == "1" || strings.EqualFold(text, "true") {
		return 1
	}
	return 0
}

func saveFIDSnapshot(data map[string]any) error {
	if storeDB == nil {
		return fmt.Errorf("sqlite store not ready")
	}
	fetchedAt := valueString(data["fetched_at"])
	if fetchedAt == "" {
		return fmt.Errorf("snapshot missing fetched_at")
	}
	flights, _ := data["flights"].([]any)

	tx, err := storeDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO fid_snapshots (fetched_at, flight_count, created_at) VALUES (?, ?, ?)`,
		fetchedAt, len(flights), utcNow()); err != nil {
		return err
	}
	var snapshotID int64
	if err := tx.QueryRow(`SELECT id FROM fid_snapshots WHERE fetched_at = ?`, fetchedAt).Scan(&snapshotID); err != nil {
		return err
	}
	for index, item := range flights {
		flight, ok := item.(map[string]any)
		if !ok {
			continue
		}
		recno := valueString(flight["recno"])
		if recno == "" {
			recno = strconv.Itoa(index)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO fid_flights (
				snapshot_id, recno, flight_number, registration, ac_type, gate,
				from_station, to_station, sta, eta, ata, std, etd, atd,
				lsodate, remarks, aog, long_turn, international
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapshotID, recno,
			valueString(flight["FlightNumber"]), valueString(flight["RegistrationNumber"]),
			valueString(flight["ACType"]), valueString(flight["GateNumber"]),
			valueString(flight["FromStation"]), valueString(flight["ToStation"]),
			valueString(flight["STA"]), valueString(flight["ETA"]), valueString(flight["ATA"]),
			valueString(flight["STD"]), valueString(flight["ETD"]), valueString(flight["ATD"]),
			valueString(flight["LSODate"]), valueString(flight["Comments_MX"]),
			flagInt(flight["AOG"]), flagInt(flight["LongTurn"]), flagInt(flight["International"]),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func saveATIS(data map[string]any) error {
	if storeDB == nil {
		return nil
	}
	airport := valueString(data["airport"])
	if airport == "" {
		return nil
	}
	code, updatedAt, text := "", "", ""
	if combined, ok := data["combined"].(map[string]any); ok && combined != nil {
		code = valueString(combined["code"])
		updatedAt = valueString(combined["updated_at"])
		text = valueString(combined["text"])
	}
	if text == "" {
		if arrival, ok := data["arrival"].(map[string]any); ok && arrival != nil {
			updatedAt = valueString(arrival["updated_at"])
			text = valueString(arrival["text"])
		}
		if departure, ok := data["departure"].(map[string]any); ok && departure != nil {
			if text == "" {
				updatedAt = valueString(departure["updated_at"])
			}
			if departureText := valueString(departure["text"]); departureText != "" {
				if text != "" {
					text += "\n\n"
				}
				text += departureText
			}
		}
	}
	_, err := storeDB.Exec(
		`INSERT INTO atis_queries (airport, source, fetch_mode, atis_code, updated_at, fetched_at, query_at, text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		airport, valueString(data["source"]), valueString(data["fetch_mode"]),
		code, updatedAt, valueString(data["fetched_at"]), utcNow(), text)
	return err
}

func handleStoreFID(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if err := saveFIDSnapshot(data); err != nil {
		log.Printf("save FID snapshot: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func newAncAPIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/atis", handleATIS)
	mux.HandleFunc("/store/fid", handleStoreFID)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	return mux
}
