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

const flightsTableSchema = `(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	flight_key TEXT NOT NULL UNIQUE,
	snapshot_id INTEGER,
	flight_number TEXT,
	lsodate TEXT,
	recno TEXT,
	registration TEXT,
	ac_type TEXT,
	gate TEXT,
	from_station TEXT,
	to_station TEXT,
	sta TEXT, eta TEXT, ata TEXT,
	std TEXT, etd TEXT, atd TEXT,
	remarks TEXT,
	aog INTEGER,
	long_turn INTEGER,
	international INTEGER,
	first_seen_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL
)`

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
	if err := ensureFlightsTable(db); err != nil {
		return err
	}
	if err := initGateSchema(db); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_fid_flights_gate ON fid_flights(gate)`,
		`CREATE INDEX IF NOT EXISTS idx_fid_flights_flight ON fid_flights(flight_number)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// ensureFlightsTable 创建新表，或把旧的按快照追加结构迁移为
// 按航班唯一（recno 优先）的结构，每个航班只保留最新一行。
func ensureFlightsTable(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='fid_flights'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err := db.Exec(`CREATE TABLE fid_flights ` + flightsTableSchema)
		return err
	}
	if _, err := db.Exec(`SELECT flight_key FROM fid_flights LIMIT 1`); err == nil {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE fid_flights_new ` + flightsTableSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO fid_flights_new (
			flight_key, snapshot_id, flight_number, lsodate, recno, registration, ac_type, gate,
			from_station, to_station, sta, eta, ata, std, etd, atd, remarks, aog, long_turn, international,
			first_seen_at, last_seen_at
		)
		SELECT
			CASE WHEN COALESCE(f.recno, '') <> '' THEN 'recno:' || f.recno
			     ELSE COALESCE(f.flight_number, '') || '|' || COALESCE(f.lsodate, '') || '|' ||
			          COALESCE(f.from_station, '') || '|' || COALESCE(f.to_station, '') END,
			f.snapshot_id, f.flight_number, f.lsodate, f.recno, f.registration, f.ac_type, f.gate,
			f.from_station, f.to_station, f.sta, f.eta, f.ata, f.std, f.etd, f.atd, f.remarks,
			f.aog, f.long_turn, f.international,
			COALESCE(s.created_at, datetime('now')), COALESCE(s.created_at, datetime('now'))
		FROM fid_flights f
		LEFT JOIN fid_snapshots s ON s.id = f.snapshot_id
		WHERE f.id IN (
			SELECT MAX(id) FROM fid_flights GROUP BY
				CASE WHEN COALESCE(recno, '') <> '' THEN 'recno:' || recno
				     ELSE COALESCE(flight_number, '') || '|' || COALESCE(lsodate, '') || '|' ||
				          COALESCE(from_station, '') || '|' || COALESCE(to_station, '') END
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE fid_flights`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE fid_flights_new RENAME TO fid_flights`); err != nil {
		return err
	}
	return tx.Commit()
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

func flightKey(flight map[string]any, index int) (string, string) {
	recno := valueString(flight["recno"])
	flightNumber := valueString(flight["FlightNumber"])
	lsodate := valueString(flight["LSODate"])
	from := valueString(flight["FromStation"])
	to := valueString(flight["ToStation"])
	if recno != "" {
		return "recno:" + recno, recno
	}
	key := flightNumber + "|" + lsodate + "|" + from + "|" + to
	if key == "|||" {
		key = fmt.Sprintf("row:%d", index)
	}
	return key, recno
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

	result, err := tx.Exec(
		`INSERT OR IGNORE INTO fid_snapshots (fetched_at, flight_count, created_at) VALUES (?, ?, ?)`,
		fetchedAt, len(flights), utcNow())
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	var snapshotID int64
	if err := tx.QueryRow(`SELECT id FROM fid_snapshots WHERE fetched_at = ?`, fetchedAt).Scan(&snapshotID); err != nil {
		return err
	}
	changes := make([]gateAlert, 0, 2)
	for index, item := range flights {
		flight, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, recno := flightKey(flight, index)
		if inserted > 0 {
			var oldGate, lastSeen string
			queryErr := tx.QueryRow(
				`SELECT COALESCE(gate, ''), COALESCE(last_seen_at, '') FROM fid_flights WHERE flight_key = ?`,
				key).Scan(&oldGate, &lastSeen)
			newGate := strings.TrimSpace(valueString(flight["GateNumber"]))
			if queryErr == nil && newGate != "" {
				oldGate = strings.TrimSpace(oldGate)
				if oldGate != "" && oldGate != newGate && lastObservationRecent(lastSeen) {
					if alert, err := recordGateAlert(tx, key, flight, oldGate, newGate, fetchedAt); err == nil {
						changes = append(changes, alert)
					}
				}
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO fid_flights (
				flight_key, snapshot_id, flight_number, lsodate, recno, registration, ac_type, gate,
				from_station, to_station, sta, eta, ata, std, etd, atd, remarks, aog, long_turn, international,
				first_seen_at, last_seen_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(flight_key) DO UPDATE SET
				snapshot_id = excluded.snapshot_id,
				flight_number = excluded.flight_number,
				lsodate = excluded.lsodate,
				recno = excluded.recno,
				registration = excluded.registration,
				ac_type = excluded.ac_type,
				gate = excluded.gate,
				from_station = excluded.from_station,
				to_station = excluded.to_station,
				sta = excluded.sta, eta = excluded.eta, ata = excluded.ata,
				std = excluded.std, etd = excluded.etd, atd = excluded.atd,
				remarks = excluded.remarks,
				aog = excluded.aog, long_turn = excluded.long_turn, international = excluded.international,
				last_seen_at = excluded.last_seen_at`,
			key, snapshotID,
			valueString(flight["FlightNumber"]), valueString(flight["LSODate"]), recno,
			valueString(flight["RegistrationNumber"]), valueString(flight["ACType"]),
			valueString(flight["GateNumber"]), valueString(flight["FromStation"]), valueString(flight["ToStation"]),
			valueString(flight["STA"]), valueString(flight["ETA"]), valueString(flight["ATA"]),
			valueString(flight["STD"]), valueString(flight["ETD"]), valueString(flight["ATD"]),
			valueString(flight["Comments_MX"]),
			flagInt(flight["AOG"]), flagInt(flight["LongTurn"]), flagInt(flight["International"]),
			fetchedAt, fetchedAt,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	enqueueGateAlerts(changes)
	return nil
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
	mux.HandleFunc("/atis/batch", handleATISBatch)
	mux.HandleFunc("/atis/history", handleATISHistory)
	mux.HandleFunc("/store/fid", handleStoreFID)
	mux.HandleFunc("/fid/history", handleFIDHistory)
	mux.HandleFunc("/gate/alerts/pending", handleGateHistoryPending)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	return mux
}
