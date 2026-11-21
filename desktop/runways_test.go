package main

import (
	"reflect"
	"testing"
)

func runwayData(combined, arrival, departure string) map[string]any {
	data := map[string]any{}
	if combined != "" {
		data["combined"] = map[string]any{"text": combined}
	}
	if arrival != "" {
		data["arrival"] = map[string]any{"text": arrival}
	}
	if departure != "" {
		data["departure"] = map[string]any{"text": departure}
	}
	annotateRunways(data)
	return data
}

func runwaysOf(data map[string]any, key string) []string {
	value, _ := data["runways"].(map[string]any)[key].([]string)
	if value == nil {
		return []string{}
	}
	return value
}

func TestRunwayExtraction(t *testing.T) {
	cases := []struct {
		name           string
		combined       string
		arrival        string
		departure      string
		wantLanding    []string
		wantDeparture  []string
	}{
		{
			name: "KLAX",
			combined: `INST APCHS AND RNAV RNP APCHS ILS RY 25L, OR VCTR FOR VISUAL APCH WILL BE PROVIDED, ` +
				`SIMUL VISUAL APCHS TO ALL RWYS ARE IN PROG, SIMUL INSTR DEPARTURES IN PROG RWYS 24 AND 25, 25R. ` +
				`RWY 2 4 LEFT 6 RIGHT CLSD, RWY 2 4 RIGHT SIX LEFT CLSD.`,
			wantLanding:   []string{"25L"},
			wantDeparture: []string{"24", "25", "25R"},
		},
		{
			name: "KORD",
			combined: `ARR EXP VECTORS ILS RWY 28R APCH. DEPS EXP RWYS 28R. ` +
				`RWY 4L, 22R CLSD, RWY 4R, 22L CLSD, RWY 9L, 27R CLSD. ` +
				`RWY 9L IM OTS, RWY 28R IM OTS.`,
			wantLanding:   []string{"28R"},
			wantDeparture: []string{"28R"},
		},
		{
			name:           "KJFK",
			combined:       `APPROACH IN USE ILS RY 31R, VISUAL APCH RY 31L. DEPG RY 31L. RWY 4R, 22L CLSD.`,
			wantLanding:    []string{"31R", "31L"},
			wantDeparture:  []string{"31L"},
		},
		{
			name:           "PANC",
			combined:       `ILS RWY 15 APPROACHES IN USE. LANDING RWY 15, DEPARTING RWY 25L. RWY 25R 5 5 5 1700Z, RWY15 5 5 5 1645Z.`,
			wantLanding:    []string{"15"},
			wantDeparture:  []string{"25L"},
		},
		{
			name:           "KMIA",
			combined:       `DEPG RWYS 27 GLADZ SID EXP 6000 FT FOR AT LEAST 45 MI. RWYS 8L, 26R CLSD, RWYS 12, 30 CLSD. RY 8L, 26R CL LGTS OTS.`,
			wantLanding:    []string{},
			wantDeparture:  []string{"27"},
		},
		{
			name:          "ZSPD",
			arrival:       `H 0800Z EXP ILS APCH INDEPENDENT PARL INSTR APCH LDG RWY 35L & 34R WIND RWY 35L TDZ 360/3MPS`,
			departure:     `E 0630Z DEP RWY 35R & 34L WIND RWY 35R TDZ 010/5MPS`,
			wantLanding:   []string{"35L", "34R"},
			wantDeparture: []string{"35R", "34L"},
		},
		{
			name:          "ZUCK",
			arrival:       `D 0500Z EXP ILSDME APCH DEPENDENT PARL INSTR APCH LDG RWY 02L & 03R TA 3000`,
			departure:     `X 2300Z DEP RWY 02R & 03L TA 3000`,
			wantLanding:   []string{"02L", "03R"},
			wantDeparture: []string{"02R", "03L"},
		},
		{
			name:          "ZGSZ",
			arrival:       `D 0854Z EXP ILS APCH 15 EXP ILS OR VIS APCH 16R WIND 070/3MPS DEPENDENT PARALLEL APPROACHES INDEPENDENT PARALLEL DEPARTURES`,
			departure:     `W 0636Z DEP RWY 33 & 34R WIND 050/2MPS`,
			wantLanding:   []string{"15", "16R"},
			wantDeparture: []string{"33", "34R"},
		},
		{
			name:          "ZGGG",
			arrival:       `S 0830Z EXP ILS OR VIS APCH INDEPENDENT PARL INSTR APCH LDG RWY 01L & 02R & 03 WIND VRB DEG 1 MPS`,
			departure:     `L 0530Z DEP RWY 01R & 02L & 03 WIND VRB DEG 3 MPS`,
			wantLanding:   []string{"01L", "02R", "03"},
			wantDeparture: []string{"01R", "02L", "03"},
		},
		{
			name:          "EHAM",
			arrival:       `M 0900Z EHAM ARRIVAL INFO M. MAIN LANDING RWY 18 R. TRANSITION LEVEL 045. RWY 18 R RCR AT TIME 0313.`,
			departure:     `Q 0900Z EHAM DEPARTURE INFO Q. MAIN DEPARTURE RWY 24. SECONDARY DEPARTURE RWY 18 L. MAIN LANDING RWY 18 R. RWY 24 RCR AT TIME 0313. RWY 18 L RCR AT TIME 0313.`,
			wantLanding:   []string{"18R"},
			wantDeparture: []string{"24", "18L"},
		},
	}

	for _, tc := range cases {
		data := runwayData(tc.combined, tc.arrival, tc.departure)
		gotLanding := runwaysOf(data, "landing")
		gotDeparture := runwaysOf(data, "departure")
		if !reflect.DeepEqual(gotLanding, tc.wantLanding) || !reflect.DeepEqual(gotDeparture, tc.wantDeparture) {
			t.Errorf("%s: got landing=%v departure=%v, want landing=%v departure=%v",
				tc.name, gotLanding, gotDeparture, tc.wantLanding, tc.wantDeparture)
		}
	}
}
