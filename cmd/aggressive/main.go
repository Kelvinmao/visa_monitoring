package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"visa_monitor/internal/booking"
	"visa_monitor/internal/config"
)

func main() {
	configPath := flag.String("config", "config.json", "Config file path")
	workers := flag.Int("workers", 0, "Number of concurrent workers (0 = use config value)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	numWorkers := cfg.WorkerCount
	if *workers > 0 {
		numWorkers = *workers
	}

	log.Printf("========================================")
	log.Printf("  AGGRESSIVE VISA BOOKING")
	log.Printf("========================================")
	log.Printf("Target Date:  %s", cfg.TargetDate)
	log.Printf("Event: %s  Plan: %s", cfg.EventID, cfg.PlanID)
	log.Printf("Release Time: %02d:%02d", cfg.ReleaseHour, cfg.ReleaseMinute)
	log.Printf("Workers:      %d", numWorkers)
	log.Printf("========================================")

	client := booking.NewAggressiveClient(cfg, numWorkers)

	log.Printf("[MAIN] Pre-warming connections...")
	client.WarmUp()

	log.Printf("[MAIN] Initialising sessions for all workers...")
	if err := client.InitAllSessions(cfg.TargetDate); err != nil {
		log.Fatalf("[MAIN] Session init failed: %v", err)
	}

	waitForRelease(cfg)

	log.Printf("[MAIN] Starting burst at %s", time.Now().Format("15:04:05.000"))
	result := client.BurstBook(cfg.TargetDate, numWorkers)

	log.Printf("========================================")
	log.Printf("RESULT:  Success=%v", result.Success)
	log.Printf("Message: %s", result.Message)
	log.Printf("Slot:    %s", result.TimeSlot)
	log.Printf("========================================")

	if result.Success {
		sendNotification(cfg, result)
	}
}

func waitForRelease(cfg *config.Config) {
	for {
		now := time.Now()
		releaseTime := cfg.GetNextReleaseTime()
		if now.Before(releaseTime) {
			until := releaseTime.Sub(now)
			if int(until.Seconds())%30 == 0 {
				log.Printf("[WAIT] %v until release", until.Round(time.Second))
			}
			time.Sleep(500 * time.Millisecond)
		} else {
			return
		}
	}
}

func sendNotification(cfg *config.Config, result *booking.Result) {
	if cfg.WebhookURL == "" {
		return
	}
	payload := map[string]interface{}{
		"success":   result.Success,
		"date":      result.Date,
		"time_slot": result.TimeSlot,
		"message":   result.Message,
	}
	data, _ := json.Marshal(payload)
	http.Post(cfg.WebhookURL, "application/json", bytes.NewReader(data))
	log.Printf("[NOTIFY] Webhook sent")
}
