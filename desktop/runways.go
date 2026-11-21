package main

import (
	"regexp"
	"strings"
)

var (
	runwayListPattern  = `(?:[0-9]{1,2}\s*[LRC]?\b)(?:\s*(?:&|,|AND|/)\s*[0-9]{1,2}\s*[LRC]?\b)*`
	runwayListRe       = regexp.MustCompile(`(?i)\b(?:RWY|RY)S?\s*(` + runwayListPattern + `)`)
	bareRunwayRe       = regexp.MustCompile(`(?i)\b(APCH|APPROACH(?:ES)?|LDG|LANDING|ARRIVAL|ARR|DEPARTURE(?:S)?|DEPARTING|DEPG|DEP|DEPS)\s+((?:[0-9]{1,2}\s*[LRC]?\b)(?:\s*(?:&|,|AND|/)\s*[0-9]{1,2}\s*[LRC]?\b)*)`)
	runwaySplitRe      = regexp.MustCompile(`(?i)\s*(?:&|,|AND|/)\s*`)
	runwayCodeRe       = regexp.MustCompile(`(?i)^([0-9]{1,2})\s*([LRC])?$`)
	landingKeywordRe   = regexp.MustCompile(`(?i)\b(?:LANDING|ARRIVAL|ARR|APPROACH|APCH|LDG|ILS|VISUAL)\b`)
	departureKeywordRe = regexp.MustCompile(`(?i)\b(?:DEPARTURE|DEPARTURES|DEPARTING|DEPG|DEP|DEPS|TAKEOFF)\b`)
	unavailableRe      = regexp.MustCompile(`(?i)\b(?:CLSD|CLOSED|OTS)\b`)
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

func nearestDirection(text string, fromEnd bool) string {
	best, bestDist := "", 1<<30
	consider := func(re *regexp.Regexp, kind string) {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			dist := loc[0]
			if fromEnd {
				dist = len(text) - loc[1]
			}
			if dist < bestDist {
				bestDist, best = dist, kind
			}
		}
	}
	consider(landingKeywordRe, "landing")
	consider(departureKeywordRe, "departure")
	return best
}

// closestDirection 优先采用跑道前最近的关键词，避免后一句的
// “DEPARTING” 抢走前一句 “LANDING RWY 15” 的方向。
func closestDirection(before, after string) string {
	if kind := nearestDirection(before, true); kind != "" {
		return kind
	}
	return nearestDirection(after, false)
}

func appendUnique(list []string, seen map[string]bool, code string) []string {
	if code == "" || seen[code] {
		return list
	}
	seen[code] = true
	return append(list, code)
}

func isUnavailable(after string) bool {
	if idx := strings.IndexAny(after, ".;"); idx >= 0 {
		after = after[:idx]
	}
	return unavailableRe.MatchString(after)
}

// extractRunways 从一段文本中提取跑道。
// source: arrival 只取落地，departure 只取起飞，combined 按关键词分类。
func extractRunways(text, source string) (landing, departure []string) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	seenLanding := map[string]bool{}
	seenDeparture := map[string]bool{}

	addList := func(list, kind string) {
		for _, part := range runwaySplitRe.Split(list, -1) {
			code := normalizeRunway(part)
			switch {
			case kind == "landing" && source != "departure":
				landing = appendUnique(landing, seenLanding, code)
			case kind == "departure" && source != "arrival":
				departure = appendUnique(departure, seenDeparture, code)
			}
		}
	}

	for _, loc := range runwayListRe.FindAllStringSubmatchIndex(text, -1) {
		listStart, listEnd := loc[2], loc[3]
		beforeStart := listStart - 80
		if beforeStart < 0 {
			beforeStart = 0
		}
		afterEnd := listEnd + 60
		if afterEnd > len(text) {
			afterEnd = len(text)
		}
		before := text[beforeStart:listStart]
		if idx := strings.LastIndexAny(before, ".;"); idx >= 0 {
			before = before[idx+1:]
		}
		after := text[listEnd:afterEnd]
		if idx := strings.IndexAny(after, ".;"); idx >= 0 {
			after = after[:idx]
		}
		if isUnavailable(after) {
			continue
		}
		kind := closestDirection(before, after)
		if kind == "" {
			switch source {
			case "arrival":
				kind = "landing"
			case "departure":
				kind = "departure"
			}
		}
		if kind != "" {
			addList(text[listStart:listEnd], kind)
		}
	}

	// 兼容 “APCH 15”“LDG 35L” 这类不带 RWY 前缀的写法
	for _, match := range bareRunwayRe.FindAllStringSubmatchIndex(text, -1) {
		keyword := strings.ToUpper(text[match[2]:match[3]])
		listStart, listEnd := match[4], match[5]
		afterEnd := listEnd + 25
		if afterEnd > len(text) {
			afterEnd = len(text)
		}
		if isUnavailable(text[listEnd:afterEnd]) {
			continue
		}
		kind := "departure"
		if landingKeywordRe.MatchString(keyword) {
			kind = "landing"
		}
		addList(text[listStart:listEnd], kind)
	}
	return landing, departure
}

// annotateRunways 从 ATIS 文本中提取落地/起飞跑道。
func annotateRunways(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	arrivalText := blockText(data["arrival"])
	departureText := blockText(data["departure"])
	combinedText := blockText(data["combined"])

	var landing, departure []string
	if combinedText != "" {
		l, d := extractRunways(combinedText, "combined")
		landing, departure = append(landing, l...), append(departure, d...)
	} else {
		l, _ := extractRunways(arrivalText, "arrival")
		_, d := extractRunways(departureText, "departure")
		landing, departure = append(landing, l...), append(departure, d...)
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
