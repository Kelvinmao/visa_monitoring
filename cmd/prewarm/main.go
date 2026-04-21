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
		log.Fatalf("Config error: %v", err)
	}

	numWorkers := cfg.WorkerCount
	if *workers > 0 {
		numWorkers = *workers
	}

	log.Printf("========================================")
	log.Printf("  PREWARM VISA BOOKING")
	log.Printf("========================================")
	log.Printf("Target:        %s", cfg.TargetDate)
	log.Printf("Release:       %02d:%02d", cfg.ReleaseHour, cfg.ReleaseMinute)
	log.Printf("Workers:       %d", numWorkers)
	log.Printf("StartEarlySec: %d", cfg.StartEarlySec)
	log.Printf("BurstDuration: %d min", cfg.BurstDuration)
	log.Printf("BaseURL:       %s", cfg.BaseURL)
	log.Printf("========================================")

	client := booking.NewPreWarmClient(cfg, numWorkers)

	releaseTime := cfg.GetNextReleaseTime()
	// Start prewarm 15 minutes early — the server is slow (~3s/request)
	// and we need enough time for retries.
	prewarmTime := releaseTime.Add(-15 * time.Minute)

	// Wait until prewarm time
	for time.Now().Before(prewarmTime) {
		until := prewarmTime.Sub(time.Now())
		if int(until.Seconds())%60 == 0 {
			log.Printf("[WAIT] %v until prewarm", until.Round(time.Second))
		}
		time.Sleep(1 * time.Second)
	}

	log.Printf("[MAIN] Starting prewarm at %s", time.Now().Format("15:04:05"))
	if err := client.PreWarm(cfg.TargetDate); err != nil {
		log.Printf("[WARN] PreWarm failed: %v — will retry with fewer workers", err)
		// Emergency fallback: try with just 3 workers
		client = booking.NewPreWarmClient(cfg, 3)
		if err := client.PreWarm(cfg.TargetDate); err != nil {
			log.Fatalf("PreWarm failed completely: %v", err)
		}
	}

	// Keep sessions alive in background
	keepAliveStop := make(chan struct{})
	go client.KeepAlive(keepAliveStop)

	log.Printf("[MAIN] Calibrating server clock....")
	booking.CalibrateServerClock(cfg.BaseURL, 30)
	clockOffset := booking.GetServerClockOffset()
	log.Printf("[MAIN] Server clock offset: %v (server is %s vs local)",
		clockOffset.Round(time.Millisecond),
		map[bool]string{true: "ahead", false: "behind"}[clockOffset > 0])

	burstStart := releaseTime.
		Add(-time.Duration(cfg.StartEarlySec) * time.Second).
		Add(-clockOffset) // compensate for server clock offset
	log.Printf("[MAIN] Burst scheduled for %s (release at %s JST, starting %ds early, clock_adj=%v)",
		burstStart.Format("15:04:05.000"), releaseTime.Format("15:04:05"),
		cfg.StartEarlySec, clockOffset.Round(time.Millisecond))
	log.Printf("[MAIN] Strategy: start %ds early, rapid-retry on 受付期間外 until server opens",
		cfg.StartEarlySec)

	keepAliveDeadline := burstStart.Add(-1 * time.Second) // stop keepalive 1s before burst

	// Wait until close to burst time.
	for time.Now().Before(keepAliveDeadline) {
		until := time.Until(burstStart)
		if int(until.Seconds())%30 == 0 {
			log.Printf("[WAIT] %v until burst", until.Round(time.Second))
		}
		time.Sleep(1 * time.Second)
	}

	// Stop keepalive now and wait for it to fully drain.
	close(keepAliveStop)
	log.Printf("[MAIN] KeepAlive stopped at %s (%.3fs before burst)",
		time.Now().Format("15:04:05.000"), time.Until(burstStart).Seconds())

	// Probe slots before burst to verify session validity
	log.Printf("[MAIN] Probing slots to verify session...")
	client.ProbeSlots(cfg.TargetDate)

	// Now spawn QuickBurst — goroutines will sleep (not busy-wait) until burstStart
	log.Printf("[MAIN] Spawning QuickBurst at %s (%.1fs before burst)",
		time.Now().Format("15:04:05.000"), time.Until(burstStart).Seconds())
	done := make(chan *booking.Result, 1)
	go func() {
		done <- client.QuickBurst(cfg.TargetDate, burstStart)
	}()

	// Sleep main goroutine until burstStart
	if sleepDur := time.Until(burstStart); sleepDur > 0 {
		time.Sleep(sleepDur)
	}

	log.Printf("[MAIN] BURST STARTED at %s", time.Now().Format("15:04:05.000"))
	result := <-done

	log.Printf("========================================")
	log.Printf("RESULT:  Success=%v", result.Success)
	log.Printf("Message: %s", result.Message)
	log.Printf("Slot:    %s", result.TimeSlot)
	log.Printf("========================================")

	if result.Success && cfg.WebhookURL != "" {
		data, _ := json.Marshal(result)
		http.Post(cfg.WebhookURL, "application/json", bytes.NewReader(data))
	}
}
