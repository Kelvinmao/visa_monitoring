package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scenario defines how the mock server behaves.
type Scenario struct {
	// Slots per time period (how many concurrent bookings allowed)
	SlotCapacity int `json:"slot_capacity"`

	// Simulated competitor count: how many "other users" grab slots
	// at the exact release moment. If CompetitorGrab >= SlotCapacity,
	// your bot will never get a slot (simulates losing the race).
	CompetitorGrab int `json:"competitor_grab"`

	// Delay before competitors grab slots (simulates their speed).
	// 0ms means instant (hardest mode).
	CompetitorDelayMs int `json:"competitor_delay_ms"`

	// Random cancellations: after X seconds, some slots reopen
	// Simulates real-world cancellations
	CancelAfterSec int `json:"cancel_after_sec"`

	// Number of slots that reopen on cancellation
	CancelCount int `json:"cancel_count"`

	// Server response latency (simulates network delay)
	ServerLatencyMs int `json:"server_latency_ms"`

	// Whether to simulate "受付期間外" before release
	EnforceReleaseTime bool `json:"enforce_release_time"`
}

var (
	// State
	csrfTokens    = make(map[string]bool)
	csrfMu        sync.RWMutex
	sessions      = make(map[string]*Session)
	slots         = make(map[string]*SlotState)
	slotsMu       sync.Mutex
	totalRequests int64
	totalSuccess  int64

	// Scenario
	scenario  Scenario
	releaseAt time.Time

	// Track which worker sessions succeeded (for analysis)
	sessionsMu sync.Mutex
)

type Session struct {
	eventID string
	planID  string
	date    string
}

type SlotState struct {
	capacity int
	taken    int
}

func randomToken() string {
	b := make([]byte, 32)
	crand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	port := flag.Int("port", 9876, "Mock server port")
	scenarioFile := flag.String("scenario", "", "JSON scenario file")
	releaseDelay := flag.Int("release-delay", 5, "Seconds until slots release")
	// Override defaults
	defaultScenario := Scenario{
		SlotCapacity:       2,
		CompetitorGrab:     50,
		CompetitorDelayMs:  0,
		CancelAfterSec:     0,
		CancelCount:        0,
		ServerLatencyMs:    50,
		EnforceReleaseTime: true,
	}

	flag.Parse()

	scenario = defaultScenario
	if *scenarioFile != "" {
		data, err := os.ReadFile(*scenarioFile)
		if err != nil {
			log.Fatalf("Failed to read scenario: %v", err)
		}
		if err := json.Unmarshal(data, &scenario); err != nil {
			log.Fatalf("Failed to parse scenario: %v", err)
		}
	}

	// Release time = now + releaseDelay seconds
	releaseAt = time.Now().Add(time.Duration(*releaseDelay) * time.Second)

	// Initialize slots
	timeSlots := []string{
		"09:00", "09:15", "09:30", "09:45",
		"10:00", "10:15", "10:30", "10:45",
		"11:00", "11:15", "11:30", "11:45",
	}
	for _, s := range timeSlots {
		slots[s] = &SlotState{capacity: scenario.SlotCapacity, taken: 0}
	}

	log.Printf("========================================")
	log.Printf("  MOCK VISA SERVER")
	log.Printf("========================================")
	log.Printf("Port:              %d", *port)
	log.Printf("SlotCapacity:      %d", scenario.SlotCapacity)
	log.Printf("CompetitorGrab:    %d", scenario.CompetitorGrab)
	log.Printf("CompetitorDelay:   %dms", scenario.CompetitorDelayMs)
	log.Printf("CancelAfterSec:    %d", scenario.CancelAfterSec)
	log.Printf("CancelCount:       %d", scenario.CancelCount)
	log.Printf("ServerLatency:     %dms", scenario.ServerLatencyMs)
	log.Printf("ReleaseTime:       %s (in ~%ds)", releaseAt.Format("15:04:05.000"), *releaseDelay)
	log.Printf("========================================")
	log.Printf("")
	log.Printf("FAIR RACE MODE: ALL requests (yours + competitors) go through")
	log.Printf("the same server latency (%dms). Competitors start at T+0 with", scenario.ServerLatencyMs)
	log.Printf("random processing delays (5-500ms). Our workers also start at T+0")
	log.Printf("with the same server latency. Fair competition!")
	log.Printf("")

	// Schedule competitors as actual HTTP clients that hit the server at T+0.
	// Each competitor has a "processing delay" that simulates their local CPU
	// time to build and send the request. This is the same delay any bot would have.
	// The server processes requests in the order they arrive.
	go func() {
		totalCompetitors := scenario.CompetitorGrab
		if totalCompetitors <= 0 {
			return
		}
		perSlot := totalCompetitors / len(slots)
		if perSlot == 0 {
			perSlot = 1
		}

		// Create HTTP clients for competitors
		competitorTransport := &http.Transport{
			MaxIdleConns:        totalCompetitors,
			MaxIdleConnsPerHost: totalCompetitors,
			MaxConnsPerHost:     totalCompetitors,
		}

		competitorID := 0
		tierCounts := map[string]int{"fast": 0, "normal": 0, "slow": 0}

		// ALL competitors start at T+0 (same time as our workers)
		// Their "delay" is how long it takes them to build and send the request
		// This simulates different bot efficiency, not network distance
		for slotName := range slots {
			for i := 0; i < perSlot; i++ {
				var processingDelay time.Duration
				r := rand.Intn(100)
				switch {
				case r < 10: // Fast bots (10%) - well optimized, 5-30ms
					processingDelay = time.Duration(5+rand.Intn(25)) * time.Millisecond
					tierCounts["fast"]++
				case r < 60: // Normal bots (50%) - average, 30-150ms
					processingDelay = time.Duration(30+rand.Intn(120)) * time.Millisecond
					tierCounts["normal"]++
				default: // Slow bots (40%) - poorly optimized, 150-500ms
					processingDelay = time.Duration(150+rand.Intn(350)) * time.Millisecond
					tierCounts["slow"]++
				}

				cid := competitorID
				competitorID++

				go func(slot string, delay time.Duration, id int) {
					time.Sleep(delay)

					optionURL := fmt.Sprintf("http://127.0.0.1:%d/reservations/option?event_id=16&event_plan_id=20&date=2026%%2F06%%2F25&time_from=%s",
						*port, slot)
					client := &http.Client{
						Transport: competitorTransport,
						CheckRedirect: func(req *http.Request, via []*http.Request) error {
							return http.ErrUseLastResponse
						},
						Timeout: 5 * time.Second,
					}

					req, _ := http.NewRequest("GET", optionURL, nil)
					req.Header.Set("User-Agent", fmt.Sprintf("CompetitorBot/%d", id))

					resp, err := client.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
				}(slotName, processingDelay, cid)
			}
		}

		log.Printf("[COMPETITORS] %d total competitors start at T+0", totalCompetitors)
		log.Printf("[COMPETITORS] Fast bots (5-30ms): %d", tierCounts["fast"])
		log.Printf("[COMPETITORS] Normal bots (30-150ms): %d", tierCounts["normal"])
		log.Printf("[COMPETITORS] Slow bots (150-500ms): %d", tierCounts["slow"])

		// Wait for all competitors to finish
		time.Sleep(1200 * time.Millisecond)
		slotsMu.Lock()
		for slotName, slot := range slots {
			log.Printf("[COMPETITORS] Slot %s: %d/%d taken", slotName, slot.taken, slot.capacity)
		}
		slotsMu.Unlock()
	}()

	// Schedule cancellations
	if scenario.CancelAfterSec > 0 && scenario.CancelCount > 0 {
		go func() {
			time.Sleep(time.Duration(scenario.CancelAfterSec) * time.Second)
			slotsMu.Lock()
			freed := 0
			for slotName, slot := range slots {
				if slot.taken > 0 && freed < scenario.CancelCount {
					slot.taken--
					freed++
					log.Printf("[CANCELLATION] Slot %s freed (now %d/%d)",
						slotName, slot.taken, slot.capacity)
				}
			}
			slotsMu.Unlock()
			log.Printf("[CANCELLATION] %d slots freed", freed)
		}()
	}

	// Stats ticker
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			reqs := atomic.LoadInt64(&totalRequests)
			succ := atomic.LoadInt64(&totalSuccess)
			log.Printf("[STATS] Total requests=%d, successes=%d", reqs, succ)
		}
	}()

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/reservations/calendar", handleCalendar)
	http.HandleFunc("/ajax/reservations/calendar", handleAjaxCalendar)
	http.HandleFunc("/reservations/option", handleOption)
	http.HandleFunc("/reservations/user/guest", handleGuest)
	http.HandleFunc("/reservations/conf", handleConf)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Mock server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func addLatency() {
	// Server processing latency: simulates real server response time
	// This applies to ALL requests equally (yours + competitors)
	if scenario.ServerLatencyMs > 0 {
		time.Sleep(time.Duration(scenario.ServerLatencyMs) * time.Millisecond)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Mock Visa Server - OK")
}

func handleCalendar(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	addLatency()

	token := randomToken()
	csrfMu.Lock()
	csrfTokens[token] = true
	csrfMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html>
<form method="post" action="/ajax/reservations/calendar">
<input type="hidden" name="_csrfToken" value="%s"/>
</form>
</html>`, token)
}

func handleAjaxCalendar(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	addLatency()

	r.ParseForm()
	csrf := r.FormValue("_csrfToken")
	csrfMu.RLock()
	valid := csrfTokens[csrf]
	csrfMu.RUnlock()
	if !valid {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Create session (use a cookie-like approach via token)
	eventID := r.FormValue("event")
	planID := r.FormValue("plan")
	date := r.FormValue("date")

	sid := randomToken()
	sessionsMu.Lock()
	sessions[sid] = &Session{eventID: eventID, planID: planID, date: date}
	sessionsMu.Unlock()

	w.Header().Set("Set-Cookie", "session="+sid)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"html":"<div>calendar</div>"}`)
}

func getSession(r *http.Request) *Session {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[cookie.Value]
}

func handleOption(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	addLatency()

	// Check release time
	if scenario.EnforceReleaseTime && time.Now().Before(releaseAt) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><title>受付期間外のため予約できません</title>
<p>受付期間外のため予約できません</p></html>`)
		return
	}

	// Check slot capacity
	slotsMu.Lock()
	timeFrom := r.URL.Query().Get("time_from")
	slot, ok := slots[timeFrom]
	if !ok {
		slotsMu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if slot.taken >= slot.capacity {
		slotsMu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><title>予約数が上限に達したため受付終了しました。</title>
<p>予約数が上限に達したため受付終了しました。</p></html>`)
		return
	}

	// Claim a slot (reserve it temporarily)
	slot.taken++
	slotsMu.Unlock()

	token := randomToken()
	csrfMu.Lock()
	csrfTokens[token] = true
	csrfMu.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<html>
<form method="post" action="/reservations/option">
<input type="hidden" name="_csrfToken" value="%s"/>
<input type="hidden" name="_Token[fields]" value="abc123"/>
<input type="hidden" name="_Token[unlocked]" value=""/>
</form>
</html>`, token)
}

func handleGuest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	addLatency()

	sess := getSession(r)
	if sess == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if r.Method == "GET" {
		token := randomToken()
		csrfMu.Lock()
		csrfTokens[token] = true
		csrfMu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html>
<form method="post" action="/reservations/user/guest">
<input type="hidden" name="_csrfToken" value="%s"/>
<input type="hidden" name="_Token[fields]" value="def456"/>
<input type="hidden" name="_Token[unlocked]" value=""/>
</form>
</html>`, token)
		return
	}

	// POST - submit guest info
	r.ParseForm()
	csrf := r.FormValue("_csrfToken")
	csrfMu.RLock()
	valid := csrfTokens[csrf]
	csrfMu.RUnlock()
	if !valid {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Redirect to confirmation
	w.Header().Set("Location", "/reservations/conf")
	w.WriteHeader(http.StatusFound)
}

func handleConf(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&totalRequests, 1)
	addLatency()

	sess := getSession(r)
	if sess == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if r.Method == "GET" {
		token := randomToken()
		csrfMu.Lock()
		csrfTokens[token] = true
		csrfMu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html>
<form method="post" action="/reservations/conf">
<input type="hidden" name="_csrfToken" value="%s"/>
<input type="hidden" name="_Token[fields]" value="ghi789"/>
<input type="hidden" name="_Token[unlocked]" value=""/>
</form>
</html>`, token)
		return
	}

	// POST - confirm booking
	r.ParseForm()
	csrf := r.FormValue("_csrfToken")
	csrfMu.RLock()
	valid := csrfTokens[csrf]
	csrfMu.RUnlock()
	if !valid {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	atomic.AddInt64(&totalSuccess, 1)

	// Success!
	w.Header().Set("Location", "/reservations/finish/12345")
	w.WriteHeader(http.StatusFound)
	log.Printf("[BOOKING SUCCESS] session=%v", sess)
}

func init() {
	_ = strings.Contains // keep strings import
}
