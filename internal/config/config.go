package config

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

type Config struct {
	TargetDate    string `json:"target_date"`
	EventID       string `json:"event_id"`
	PlanID        string `json:"plan_id"`
	FamilyName    string `json:"family_name"`
	FirstName     string `json:"first_name"`
	Phone         string `json:"phone"`
	Email         string `json:"email"`
	ReleaseHour   int    `json:"release_hour"`
	ReleaseMinute int    `json:"release_minute"`
	StartEarlySec int    `json:"start_early_sec"`
	BurstDuration int    `json:"burst_duration_min"`
	WorkerCount   int    `json:"worker_count"`

	BaseURL      string `json:"base_url"`
	MasterPort   int    `json:"master_port"`
	ScanInterval int    `json:"scan_interval"`
	ScanDays     int    `json:"scan_days"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMs int    `json:"retry_delay_ms"`

	WebhookURL string `json:"webhook_url"`
	LogFile    string `json:"log_file"`
}

var defaultConfig = Config{
	BaseURL:       "https://toronto.rsvsys.jp",
	MasterPort:    8080,
	ScanInterval:  15,
	ScanDays:      90,
	MaxRetries:    1000,
	RetryDelayMs:  150,
	ReleaseHour:   20,
	ReleaseMinute: 0,
	StartEarlySec: 60,
	BurstDuration: 30,
	WorkerCount:   3,
}

func Load(configPath string) (*Config, error) {
	cfg := defaultConfig

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
		}
	}

	if v := os.Getenv("VISA_TARGET_DATE"); v != "" {
		cfg.TargetDate = v
	}
	if v := os.Getenv("VISA_EVENT_ID"); v != "" {
		cfg.EventID = v
	}
	if v := os.Getenv("VISA_PLAN_ID"); v != "" {
		cfg.PlanID = v
	}
	if v := os.Getenv("VISA_FAMILY_NAME"); v != "" {
		cfg.FamilyName = v
	}
	if v := os.Getenv("VISA_FIRST_NAME"); v != "" {
		cfg.FirstName = v
	}
	if v := os.Getenv("VISA_PHONE"); v != "" {
		cfg.Phone = v
	}
	if v := os.Getenv("VISA_EMAIL"); v != "" {
		cfg.Email = v
	}
	if v := os.Getenv("VISA_RELEASE_HOUR"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.ReleaseHour = i
		}
	}
	if v := os.Getenv("VISA_RELEASE_MINUTE"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.ReleaseMinute = i
		}
	}
	if v := os.Getenv("VISA_START_EARLY_SEC"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.StartEarlySec = i
		}
	}
	if v := os.Getenv("VISA_WEBHOOK_URL"); v != "" {
		cfg.WebhookURL = v
	}
	if v := os.Getenv("VISA_MASTER_PORT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MasterPort = i
		}
	}

	return &cfg, nil
}

func (c *Config) GetTargetDateTime() time.Time {
	if c.TargetDate == "" {
		return time.Now().AddDate(0, 0, 60)
	}
	t, err := time.Parse("2006/01/02", c.TargetDate)
	if err != nil {
		return time.Now().AddDate(0, 0, 60)
	}
	return t
}

func releaseLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.FixedZone("JST", 9*60*60)
	}
	return loc
}

func (c *Config) GetNextReleaseTime() time.Time {
	loc := releaseLocation()
	now := time.Now().In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), c.ReleaseHour, c.ReleaseMinute, 0, 0, loc)
	gracePeriod := time.Duration(c.StartEarlySec+30) * time.Second
	if now.After(target.Add(gracePeriod)) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}

func (c *Config) GetStartTime() time.Time {
	return c.GetNextReleaseTime().Add(-time.Duration(c.StartEarlySec) * time.Second)
}

func (c *Config) GetEndTime() time.Time {
	return c.GetNextReleaseTime().Add(time.Duration(c.BurstDuration) * time.Minute)
}
