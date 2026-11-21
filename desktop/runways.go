package main

import (
	"regexp"
	"strings"
)

var (
	runwayListPattern = `(?:[0-9]{1,2}\s*[LRC]?)(?:\s*(?:&|,|AND|/)\s*[0-9]{1,2}\s*[LRC]?)*`
	landingRunwayRe   = regexp.MustCompile(`(?i)(?:LANDING|ARRIVAL|APPROACH(?:ES)?|APCH|LDG)[^.]{0,90}?\b(?:RWY|RY)S?\s*(` + runwayListPattern + `)`)
	departureRunwayRe = regexp.MustCompile(`(?i)(?:DEPARTURES?|DEPARTING|DEPG|DEP)\b[^.]{0,60}?\b(?:RWY|RY)S?\s*(` + runwayListPattern + `)`)
	runwaySplitRe     = regexp.MustCompile(`(?i)\s*(?:&|,|AND|/)\s*`)
	runwayCodeRe      = regexp.MustCompile(`(?i)^([0-9]{1,2})\s*([LRC])?$`)
)

func normalizeRunway(raw string) string {
	match := runwayCodeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return ""
	}
	code := match[1]
	if side := strings.ToUpper(match[2]); side != "" {
		code += side
	}
	return code
}

func blockText(value any) string {
	if block, ok := value.(map[string]any); ok {
		if text, ok := block["text"].(string); ok {
			return text
		}
	}
	return ""
}

func extractRunways(pattern *regexp.Regexp, text string) []string {
	seen := map[string]bool{}
	runways := []string{}
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		for _, part := range runwaySplitRe.Split(match[1], -1) {
			code := normalizeRunway(part)
			if code != "" && !seen[code] {
				seen[code] = true
				runways = append(runways, code)
			}
		}
	}
	return runways
}

// annotateRunways 从 ATIS 文本中提取落地/起飞跑道。
// combined 机场从合并文本提取；否则落地取 arrival、起飞取 departure。
func annotateRunways(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	arrivalText := blockText(data["arrival"])
	departureText := blockText(data["departure"])
	combinedText := blockText(data["combined"])

	var landing, departure []string
	if combinedText != "" {
		landing = extractRunways(landingRunwayRe, combinedText)
		departure = extractRunways(departureRunwayRe, combinedText)
	} else {
		landing = extractRunways(landingRunwayRe, arrivalText)
		departure = extractRunways(departureRunwayRe, departureText)
	}
	data["runways"] = map[string]any{
		"landing":   landing,
		"departure": departure,
	}
	return data
}

func finalizeATIS(data map[string]any, code string) map[string]any {
	return annotateRunways(annotateAirport(data, code))
}
