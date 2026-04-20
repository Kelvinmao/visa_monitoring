package main

import (
	"flag"
	"log"
	"time"

	"visa_monitor/internal/booking"
	"visa_monitor/internal/config"
)

// trybook: immediately prewarm + burst, no waiting for release time.
// Usage: go run cmd/trybook/main.go -config config_test_booking.json
func main() {
	configPath := flag.String("config", "config_test_booking.json", "Config file path")
	workers := flag.Int("workers", 0, "Number of workers (0 = use config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	numWorkers := cfg.WorkerCount
	if *workers > 0 {
		numWorkers = *workers
	}

	log.Printf("========================================")
	log.Printf("  TRYBOOK — immediate booking test")
	log.Printf("========================================")
	log.Printf("Target: %s", cfg.TargetDate)
	log.Printf("Event:  %s  Plan: %s", cfg.EventID, cfg.PlanID)
	log.Printf("Workers: %d", numWorkers)
	log.Printf("Name:   %s %s", cfg.FamilyName, cfg.FirstName)
	log.Printf("========================================")

	client := booking.NewPreWarmClient(cfg, numWorkers)

	log.Printf("[TRYBOOK] Pre-warming sessions...")
	if err := client.PreWarm(cfg.TargetDate); err != nil {
		log.Fatalf("[TRYBOOK] PreWarm failed: %v", err)
	}
	log.Printf("[TRYBOOK] PreWarm done, starting burst immediately")

	result := client.QuickBurst(cfg.TargetDate, time.Now())

	log.Printf("========================================")
	log.Printf("RESULT:  Success=%v", result.Success)
	log.Printf("Message: %s", result.Message)
	log.Printf("Slot:    %s", result.TimeSlot)
	log.Printf("========================================")
}
