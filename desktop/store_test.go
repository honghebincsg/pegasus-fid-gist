package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestFlightsUpsertUnique(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	storeDB = db
	defer func() { storeDB = nil }()

	first := map[string]any{
		"fetched_at": "2026-11-21 10:00:00Z",
		"flights": []any{
			map[string]any{"recno": "1001", "FlightNumber": "CZ1", "LSODate": "2026-11-21",
				"GateNumber": "A1", "FromStation": "CAN", "ToStation": "ANC", "STA": "10:30"},
		},
	}
	second := map[string]any{
		"fetched_at": "2026-11-21 10:05:00Z",
		"flights": []any{
			map[string]any{"recno": "1001", "FlightNumber": "CZ1", "LSODate": "2026-11-21",
				"GateNumber": "A2", "FromStation": "CAN", "ToStation": "ANC", "STA": "10:40"},
		},
	}
	if err := saveFIDSnapshot(first); err != nil {
		t.Fatal(err)
	}
	if err := saveFIDSnapshot(second); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fid_flights`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one unique flight, got %d", count)
	}
	var gate, firstSeen, lastSeen string
	if err := db.QueryRow(`SELECT gate, first_seen_at, last_seen_at FROM fid_flights WHERE flight_key = 'recno:1001'`).
		Scan(&gate, &firstSeen, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if gate != "A2" {
		t.Fatalf("expected gate overwritten to A2, got %s", gate)
	}
	if firstSeen != "2026-11-21 10:00:00Z" || lastSeen != "2026-11-21 10:05:00Z" {
		t.Fatalf("unexpected seen range: %s .. %s", firstSeen, lastSeen)
	}
	var snapshots int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fid_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 {
		t.Fatalf("expected two snapshots, got %d", snapshots)
	}
}
