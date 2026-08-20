package notify

import (
	"fmt"
	"strings"
	"time"
)

// ValidatePaceConfig validates a user-supplied work schedule and thresholds.
func ValidatePaceConfig(cfg PaceConfig) error {
	if cfg.Warning < 0 || cfg.Warning > 100 || cfg.Critical < 0 || cfg.Critical > 100 || cfg.Warning >= cfg.Critical {
		return fmt.Errorf("pace warning threshold must be less than critical threshold and both must be between 0 and 100")
	}
	if cfg.WorkdaysPerWeek < 1 || cfg.WorkdaysPerWeek > 7 {
		return fmt.Errorf("workdays per week must be between 1 and 7")
	}
	if cfg.LunchMinutes < 0 || cfg.LunchMinutes > 240 {
		return fmt.Errorf("lunch duration must be between 0 and 240 minutes")
	}
	startH, startM, startOK := parseClock(cfg.WorkdayStart)
	endH, endM, endOK := parseClock(cfg.WorkdayEnd)
	lunchH, lunchM, lunchOK := parseClock(cfg.LunchStart)
	if !startOK || !endOK || !lunchOK {
		return fmt.Errorf("workday and lunch times must use HH:MM format")
	}
	startMinutes, endMinutes, lunchMinutes := startH*60+startM, endH*60+endM, lunchH*60+lunchM
	if endMinutes <= startMinutes {
		return fmt.Errorf("workday end must be later than workday start")
	}
	if cfg.LunchMinutes > 0 && (lunchMinutes < startMinutes || lunchMinutes+cfg.LunchMinutes > endMinutes) {
		return fmt.Errorf("lunch break must fit inside the workday")
	}
	return nil
}

func normalizePaceConfig(cfg PaceConfig) PaceConfig {
	if cfg.Warning < 0 || (cfg.Warning == 0 && cfg.Critical == 0) {
		cfg.Warning = 10
	}
	if cfg.Critical <= cfg.Warning {
		cfg.Critical = 20
	}
	if cfg.WorkdayStart == "" {
		cfg.WorkdayStart = "09:00"
	}
	if cfg.LunchStart == "" {
		cfg.LunchStart = "12:00"
	}
	if cfg.LunchMinutes < 0 || cfg.LunchMinutes > 240 {
		cfg.LunchMinutes = 60
	}
	if cfg.WorkdayEnd == "" {
		cfg.WorkdayEnd = "18:00"
	}
	if cfg.WorkdaysPerWeek < 1 || cfg.WorkdaysPerWeek > 7 {
		cfg.WorkdaysPerWeek = 5
	}
	return cfg
}

func quotaAlertMetric(status QuotaStatus, cfg PaceConfig, now time.Time) (metric, workProgress float64, evaluated bool) {
	if !cfg.Enabled || status.ResetsAt == nil {
		return status.Utilization, 0, false
	}
	location := now.Location()
	if cfg.Timezone != "" {
		if configuredLocation, err := time.LoadLocation(cfg.Timezone); err == nil {
			location = configuredLocation
		}
	}
	now = now.In(location)
	resetAt := status.ResetsAt.In(location)
	key := strings.ToLower(strings.TrimSpace(status.QuotaKey))
	var start time.Time
	switch {
	case key == "weekly" || key == "seven_day" || strings.HasPrefix(key, "weekly_") || strings.HasPrefix(key, "seven_day_"):
		start = resetAt.AddDate(0, 0, -7)
	case key == "monthly" || key == "monthly_limit" || strings.HasPrefix(key, "monthly_"):
		start = resetAt.AddDate(0, -1, 0)
	default:
		return status.Utilization, 0, false
	}
	cfg = normalizePaceConfig(cfg)
	workProgress = workProgressPercent(start, resetAt, now, cfg)
	return status.Utilization - workProgress, workProgress, true
}

// PaceMarkers returns the warning and critical utilization positions used by
// weekly and monthly pace alerts. The values are suitable for rendering on a
// 0-100% quota bar and deliberately share the notification engine's schedule
// calculation so the dashboard cannot drift from alert behavior.
func PaceMarkers(quotaKey string, resetsAt time.Time, cfg PaceConfig, now time.Time) (warning, critical, workProgress float64, evaluated bool) {
	_, workProgress, evaluated = quotaAlertMetric(QuotaStatus{QuotaKey: quotaKey, ResetsAt: &resetsAt}, cfg, now)
	if !evaluated {
		return 0, 0, 0, false
	}
	cfg = normalizePaceConfig(cfg)
	warning = min(100, workProgress+cfg.Warning)
	critical = min(100, workProgress+cfg.Critical)
	return warning, critical, workProgress, true
}

func parseClock(value string) (hour, minute int, ok bool) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, 0, false
	}
	return parsed.Hour(), parsed.Minute(), true
}

func clockAt(day time.Time, value string) (time.Time, bool) {
	h, m, ok := parseClock(value)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location()), true
}

func isConfiguredWorkday(day time.Time, workdays int) bool {
	weekday := int(day.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	return weekday <= workdays
}

func overlapDuration(startA, endA, startB, endB time.Time) time.Duration {
	start := startA
	if startB.After(start) {
		start = startB
	}
	end := endA
	if endB.Before(end) {
		end = endB
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

func workingDuration(start, end time.Time, cfg PaceConfig) time.Duration {
	if !end.After(start) {
		return 0
	}
	cfg = normalizePaceConfig(cfg)
	loc := start.Location()
	end = end.In(loc)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	last := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, loc)
	var total time.Duration
	for !day.After(last) {
		if isConfiguredWorkday(day, cfg.WorkdaysPerWeek) {
			workStart, okStart := clockAt(day, cfg.WorkdayStart)
			workEnd, okEnd := clockAt(day, cfg.WorkdayEnd)
			lunchStart, okLunch := clockAt(day, cfg.LunchStart)
			if okStart && okEnd && workEnd.After(workStart) {
				total += overlapDuration(start, end, workStart, workEnd)
				if okLunch && cfg.LunchMinutes > 0 {
					lunchEnd := lunchStart.Add(time.Duration(cfg.LunchMinutes) * time.Minute)
					total -= overlapDuration(start, end, lunchStart, lunchEnd)
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	if total < 0 {
		return 0
	}
	return total
}

func workProgressPercent(start, end, now time.Time, cfg PaceConfig) float64 {
	if !now.After(start) {
		return 0
	}
	if now.After(end) {
		now = end
	}
	total := workingDuration(start, end, cfg)
	if total <= 0 {
		return 0
	}
	elapsed := workingDuration(start, now, cfg)
	progress := float64(elapsed) / float64(total) * 100
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}
