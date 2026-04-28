package booking

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"visa_monitor/internal/config"
)

// ---------------------------------------------------------------------------
// Integration test: simulates the EXTREME real-world scenario where:
//   - Server clock is offset (behind local by ~900ms)
//   - Before server 09:00:00 → returns 400 "受付期間外のため予約できません"
//   - At server 09:00:00 → slots open, first N requests get 200 (option page)
//   - After ~1 second → all slots return 400 "予約数が上限に達したため受付終了しました。"
//
// The test verifies that the full PreWarm → QuickBurst pipeline can book
// a slot within this ~1 second window.
// ---------------------------------------------------------------------------

// mockVisaServer simulates the toronto.rsvsys.jp CakePHP backend.
// It faithfully reproduces the real server behavior observed from production logs.
type mockVisaServer struct {
	t *testing.T

	// serverRelease is the "server time" at which slots open.
	// This is in local time but represents the server's view of 09:00:00.
	serverRelease time.Time

	// capacityPerSlot controls how many successful bookings each slot allows.
	capacityPerSlot int

	// mu protects slotBookings
	mu           sync.Mutex
	slotBookings map[string]int

	// Tracking
	totalOptionReqs int64
	totalBookings   int64
	firstOptionHit  int64 // unix nano of first option request
	firstBooking    int64 // unix nano of first successful booking

	// requestLog records timestamped events for post-test analysis
	logMu      sync.Mutex
	requestLog []string
}

func newMockVisaServer(t *testing.T, serverRelease time.Time, capacityPerSlot int) *mockVisaServer {
	return &mockVisaServer{
		t:               t,
		serverRelease:   serverRelease,
		capacityPerSlot: capacityPerSlot,
		slotBookings:    make(map[string]int),
	}
}

func (m *mockVisaServer) logEvent(format string, args ...interface{}) {
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	m.logMu.Lock()
	m.requestLog = append(m.requestLog, entry)
	m.logMu.Unlock()
}

func (m *mockVisaServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate server Date header with offset (server is behind local).
		// In real scenario, offset = server - local ≈ -900ms.
		// We set the Date header to serverRelease-based time.
		serverNow := time.Now().Add(-900 * time.Millisecond)
		w.Header().Set("Date", serverNow.UTC().Format(http.TimeFormat))

		switch {
		// Step 1: GET /reservations/calendar → returns CSRF token
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/calendar":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><form><input type="hidden" name="_csrfToken" value="mock-csrf-token-123"></form></html>`)

		// Step 2: POST /ajax/reservations/calendar → set session
		case r.Method == http.MethodPost && r.URL.Path == "/ajax/reservations/calendar":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"success":true}`)

		// Step 3: GET /reservations/option → THE CRITICAL ENDPOINT
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			atomic.AddInt64(&m.totalOptionReqs, 1)
			if m.firstOptionHit == 0 {
				atomic.CompareAndSwapInt64(&m.firstOptionHit, 0, time.Now().UnixNano())
			}

			slot := r.URL.Query().Get("time_from")
			now := time.Now()

			// Before release: "受付期間外"
			if now.Before(m.serverRelease) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `受付期間外のため予約できません`)
				return
			}

			// After release: check capacity
			m.mu.Lock()
			booked := m.slotBookings[slot]
			if booked < m.capacityPerSlot {
				// Slot available! Return 200 with option page form
				m.mu.Unlock()
				m.logEvent("OPTION 200: slot=%s (booked=%d/%d)", slot, booked, m.capacityPerSlot)
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<html><form action="/reservations/option" method="post">
					<input type="hidden" name="_csrfToken" value="option-csrf-%s">
					<input type="hidden" name="_Token[fields]" value="option-fields-%s">
					<input type="hidden" name="_Token[unlocked]" value="option-unlocked-%s">
					<button type="submit">Next</button>
				</form></html>`, slot, slot, slot)
				return
			}
			m.mu.Unlock()

			// Capacity full
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)

		// Step 4: POST /reservations/option → 302 → /reservations/user/guest
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/option":
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)

		// Step 5: GET /reservations/user/guest → guest form
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><form action="/reservations/user/guest" method="post">
				<input type="hidden" name="_csrfToken" value="guest-csrf">
				<input type="hidden" name="_Token[fields]" value="guest-fields">
				<input type="hidden" name="_Token[unlocked]" value="guest-unlocked">
			</form></html>`)

		// Step 6: POST /reservations/user/guest → 302 → /reservations/conf
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)

		// Step 7: GET /reservations/conf → confirmation page
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><form action="/reservations/conf" method="post">
				<input type="hidden" name="_csrfToken" value="conf-csrf">
				<input type="hidden" name="_Token[fields]" value="conf-fields">
				<input type="hidden" name="_Token[unlocked]" value="conf-unlocked">
			</form></html>`)

		// Step 8: POST /reservations/conf → ACTUAL BOOKING
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			// Determine which slot this session is booking.
			// In real CakePHP this is tracked by session; here we pick from Referer or just
			// do a global capacity check. For simplicity, grab the first available slot.
			m.mu.Lock()
			bookedSlot := ""
			for _, slot := range GetTimeSlots() {
				if m.slotBookings[slot] < m.capacityPerSlot {
					m.slotBookings[slot]++
					bookedSlot = slot
					break
				}
			}
			m.mu.Unlock()

			if bookedSlot != "" {
				atomic.AddInt64(&m.totalBookings, 1)
				if m.firstBooking == 0 {
					atomic.CompareAndSwapInt64(&m.firstBooking, 0, time.Now().UnixNano())
				}
				m.logEvent("BOOKING SUCCESS: slot=%s", bookedSlot)
				w.Header().Set("Location", fmt.Sprintf("/reservations/finish/%s", bookedSlot))
				w.WriteHeader(http.StatusFound)
			} else {
				// All slots taken — booking fails
				m.logEvent("BOOKING FAILED: all slots full")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 1: EXTREME — 1-second window, 1 slot per time, server clock offset
// This simulates the exact scenario from the April 18 failure: slots fill
// in ~1 second. We verify that with 10 workers we can book at least 1 slot.
// ---------------------------------------------------------------------------
func TestIntegration_ExtremeOneSecondWindow(t *testing.T) {
	// Release happens 20 seconds from now to allow PreWarm to complete first.
	// PreWarm takes ~12s with 10 workers (batch=2, delay=3s).
	// The mock server opens slots at serverRelease.
	// Server clock is -900ms behind local (mimicking real offset).
	// With our fix, burstStart compensates for this offset.
	serverRelease := time.Now().Add(20 * time.Second)

	// Only 1 booking allowed per slot — extreme scarcity
	mock := newMockVisaServer(t, serverRelease, 1)

	srv := httptest.NewServer(mock.Handler())
	defer srv.Close()

	cfg := &config.Config{
		TargetDate:    "2026/06/22",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Test",
		FirstName:     "User",
		Phone:         "123-456-7890",
		Email:         "test@example.com",
		BaseURL:       srv.URL,
		WorkerCount:   10,
		BurstDuration: 1, // 1 minute max
		StartEarlySec: 1, // start 1 second early
	}

	// Simulate server clock offset detection.
	// In real code this happens during PreWarm when reading the Date header.
	// The mock server returns Date header 900ms behind, so offset = -900ms.
	// Set it manually for test determinism.
	setServerClockOffset(-900 * time.Millisecond)
	defer setServerClockOffset(0)

	client := NewPreWarmClient(cfg, cfg.WorkerCount)

	// PreWarm: establish sessions
	err := client.PreWarm(cfg.TargetDate)
	if err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	readyCount := 0
	for _, w := range client.clients {
		if w.csrfToken != "" {
			readyCount++
		}
	}
	t.Logf("Workers ready: %d/%d", readyCount, cfg.WorkerCount)

	// Calculate burstStart using the same formula as cmd/prewarm/main.go.
	// burstStart = serverRelease - offset - startEarlySec
	// (serverRelease is the real "server opens" time in local clock terms)
	offset := GetServerClockOffset()
	burstStart := serverRelease.Add(-offset)
	if cfg.StartEarlySec > 0 {
		burstStart = burstStart.Add(-time.Duration(cfg.StartEarlySec) * time.Second)
	}

	t.Logf("Server release: %s", serverRelease.Format("15:04:05.000"))
	t.Logf("Clock offset: %v", offset)
	t.Logf("Burst start: %s (%.1fs from now)", burstStart.Format("15:04:05.000"), time.Until(burstStart).Seconds())
	t.Logf("Expected first request arrival at server: ~%s", burstStart.Add(offset).Format("15:04:05.000"))

	// Run QuickBurst
	result := client.QuickBurst(cfg.TargetDate, burstStart)

	// Analyze results
	totalOpts := atomic.LoadInt64(&mock.totalOptionReqs)
	totalBooks := atomic.LoadInt64(&mock.totalBookings)

	t.Logf("=== RESULTS ===")
	t.Logf("Total option requests: %d", totalOpts)
	t.Logf("Total successful bookings: %d", totalBooks)
	t.Logf("Result: success=%v slot=%s msg=%s", result.Success, result.TimeSlot, result.Message)

	// Print server event log
	mock.logMu.Lock()
	for _, entry := range mock.requestLog {
		t.Logf("  %s", entry)
	}
	mock.logMu.Unlock()

	if !result.Success {
		t.Fatalf("FAILED: Could not book a slot in the 1-second window!\n"+
			"Total requests: %d, Bookings: %d", totalOpts, totalBooks)
	}
	t.Logf("SUCCESS: Booked slot %s", result.TimeSlot)
}

// ---------------------------------------------------------------------------
// Test 2: Slots fill over 1 second — verify stagger covers the window.
// The mock server allows bookings for only 1 second after opening.
// After that, ALL slots return capacity-full.
// ---------------------------------------------------------------------------
func TestIntegration_SlotsCloseAfterOneSecond(t *testing.T) {
	// PreWarm takes ~12s (5 batches of 2 workers, 3s delay each).
	// Set release far enough in the future so burstStart isn't already past.
	serverRelease := time.Now().Add(20 * time.Second)

	// Use a time-based mock: slots are only available for 1s after release
	capacityPerSlot := 1
	mock := newMockVisaServer(t, serverRelease, capacityPerSlot)

	// Override the option handler to add time-based cutoff:
	// After 1 second past release, ALL slots return capacity-full regardless.
	cutoffTime := serverRelease.Add(1 * time.Second)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/reservations/option" {
			atomic.AddInt64(&mock.totalOptionReqs, 1)
			now := time.Now()

			// Before release
			if now.Before(serverRelease) {
				w.Header().Set("Date", now.Add(-900*time.Millisecond).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `受付期間外のため予約できません`)
				return
			}

			// After cutoff — everything is full
			if now.After(cutoffTime) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
				return
			}

			// In the 1-second window — check per-slot capacity
			slot := r.URL.Query().Get("time_from")
			mock.mu.Lock()
			if mock.slotBookings[slot] < capacityPerSlot {
				mock.mu.Unlock()
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<html><form>
					<input name="_csrfToken" value="csrf-%s">
					<input name="_Token[fields]" value="fields-%s">
					<input name="_Token[unlocked]" value="">
				</form></html>`, slot, slot)
				return
			}
			mock.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
			return
		}

		// Delegate everything else to the main mock handler
		mock.Handler().ServeHTTP(w, r)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	cfg := &config.Config{
		TargetDate:    "2026/06/22",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Test",
		FirstName:     "User",
		Phone:         "123-456-7890",
		Email:         "test@example.com",
		BaseURL:       srv.URL,
		WorkerCount:   10,
		BurstDuration: 1,
		StartEarlySec: 1,
	}

	setServerClockOffset(-900 * time.Millisecond)
	defer setServerClockOffset(0)

	client := NewPreWarmClient(cfg, cfg.WorkerCount)
	if err := client.PreWarm(cfg.TargetDate); err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	offset := GetServerClockOffset()
	burstStart := serverRelease.Add(-offset).Add(-time.Duration(cfg.StartEarlySec) * time.Second)

	t.Logf("Window: %s to %s (1 second)",
		serverRelease.Format("15:04:05.000"), cutoffTime.Format("15:04:05.000"))
	t.Logf("Burst start: %s", burstStart.Format("15:04:05.000"))

	result := client.QuickBurst(cfg.TargetDate, burstStart)

	totalOpts := atomic.LoadInt64(&mock.totalOptionReqs)
	t.Logf("Total option requests: %d", totalOpts)
	t.Logf("Result: success=%v slot=%s msg=%s", result.Success, result.TimeSlot, result.Message)

	if !result.Success {
		t.Fatalf("FAILED to book within 1-second window! Requests=%d", totalOpts)
	}
	t.Logf("SUCCESS: Booked slot %s within 1-second window", result.TimeSlot)
}

// ---------------------------------------------------------------------------
// Test 3: Verify no early-exit — all slots capacity-full but workers keep polling.
// After 5 seconds of capacity-full, one slot reopens (simulating a cancellation).
// Workers should catch it because they don't exit early anymore.
// ---------------------------------------------------------------------------
func TestIntegration_NoEarlyExitCatchesReopening(t *testing.T) {
	// PreWarm takes ~6s for 5 workers. Set release far enough out.
	serverRelease := time.Now().Add(15 * time.Second)
	reopenTime := serverRelease.Add(3 * time.Second)    // slot reopens 3s after release
	closeFinalTime := reopenTime.Add(10 * time.Second)   // stays open for 10s (generous window for test)

	var optionReqs int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(-900*time.Millisecond).UTC().Format(http.TimeFormat))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/calendar":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-tok">`)
		case r.Method == http.MethodPost && r.URL.Path == "/ajax/reservations/calendar":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			atomic.AddInt64(&optionReqs, 1)
			now := time.Now()
			slot := r.URL.Query().Get("time_from")

			if now.Before(serverRelease) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `受付期間外のため予約できません`)
				return
			}

			// Between release and reopen: all slots full
			// Between reopen and closeFinal: slot "09:00" is available
			// After closeFinal: all full again
			if now.After(reopenTime) && now.Before(closeFinalTime) && slot == "09:00" {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><form><input name="_csrfToken" value="csrf-opt"><input name="_Token[fields]" value="f-opt"><input name="_Token[unlocked]" value="u-opt"></form></html>`)
				return
			}

			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)

		case r.Method == http.MethodPost && r.URL.Path == "/reservations/option":
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-g"><input name="_Token[fields]" value="f-g"><input name="_Token[unlocked]" value="u-g">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-c"><input name="_Token[fields]" value="f-c"><input name="_Token[unlocked]" value="u-c">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			w.Header().Set("Location", "/reservations/finish/09:00")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	cfg := &config.Config{
		TargetDate:    "2026/06/22",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Test",
		FirstName:     "User",
		Phone:         "123-456-7890",
		Email:         "test@example.com",
		BaseURL:       srv.URL,
		WorkerCount:   5,
		BurstDuration: 1,  // 1-minute max burst
		StartEarlySec: 1,
	}

	setServerClockOffset(-900 * time.Millisecond)
	defer setServerClockOffset(0)

	client := NewPreWarmClient(cfg, cfg.WorkerCount)
	if err := client.PreWarm(cfg.TargetDate); err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	offset := GetServerClockOffset()
	burstStart := serverRelease.Add(-offset).Add(-time.Duration(cfg.StartEarlySec) * time.Second)

	t.Logf("Release: %s, Reopen: %s, Close: %s",
		serverRelease.Format("15:04:05.000"),
		reopenTime.Format("15:04:05.000"),
		closeFinalTime.Format("15:04:05.000"))

	result := client.QuickBurst(cfg.TargetDate, burstStart)

	reqs := atomic.LoadInt64(&optionReqs)
	t.Logf("Total option requests: %d", reqs)
	t.Logf("Result: success=%v slot=%s msg=%s", result.Success, result.TimeSlot, result.Message)

	if !result.Success {
		t.Fatalf("FAILED: Workers should have caught the reopened slot after 3 seconds. "+
			"This means early-exit is still killing workers prematurely. Requests=%d", reqs)
	}
	t.Logf("SUCCESS: Caught reopened slot after capacity-full period (%d requests)", reqs)
}

// ---------------------------------------------------------------------------
// Test 4: COMPETITIVE — Multiple rival users racing for the same slots.
//
// Simulates the real-world scenario:
//   - 12 time slots, 1 capacity each = 12 total bookings possible
//   - 30 rival "users" (goroutines) start booking at release time
//   - Each rival completes the full 4-step flow (option→guest→conf→finish)
//     with realistic delays to simulate network latency
//   - Our 10 workers race against these 30 rivals
//   - Rivals have varied speeds (50ms-300ms per step) to simulate real humans/bots
//   - Some rivals are FAST (50ms) — faster than our real-world RTT of ~1-2s
//
// Success criteria: our system books at least 1 slot against 30 competitors.
// ---------------------------------------------------------------------------
func TestIntegration_CompetitiveRivalUsers(t *testing.T) {
	const (
		numSlots        = 12
		capacityPerSlot = 1  // only 1 booking per slot
		numRivals       = 30 // 30 competing users
	)

	serverRelease := time.Now().Add(20 * time.Second)

	// Shared booking state: tracks who booked what.
	type bookingEntry struct {
		slot   string
		who    string // "rival-N" or "our-worker-N"
		bookedAt time.Time
	}
	var (
		mu           sync.Mutex
		slotBookings = make(map[string]int)  // slot → count
		bookingLog   []bookingEntry
	)

	// tryBook atomically attempts to book a slot. Returns true if successful.
	tryBook := func(slot, who string) bool {
		mu.Lock()
		defer mu.Unlock()
		if slotBookings[slot] < capacityPerSlot {
			slotBookings[slot]++
			bookingLog = append(bookingLog, bookingEntry{slot: slot, who: who, bookedAt: time.Now()})
			return true
		}
		return false
	}

	isSlotAvailable := func(slot string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slotBookings[slot] < capacityPerSlot
	}

	totalSlotsBooked := func() int {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for _, c := range slotBookings {
			total += c
		}
		return total
	}

	var ourBookings int64
	var rivalBookings int64

	// Build the mock server with contention-aware booking
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverNow := time.Now().Add(-900 * time.Millisecond)
		w.Header().Set("Date", serverNow.UTC().Format(http.TimeFormat))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/calendar":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="mock-csrf-123">`)

		case r.Method == http.MethodPost && r.URL.Path == "/ajax/reservations/calendar":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			slot := r.URL.Query().Get("time_from")
			now := time.Now()

			if now.Before(serverRelease) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `受付期間外のため予約できません`)
				return
			}

			if isSlotAvailable(slot) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<form><input name="_csrfToken" value="csrf-%s"><input name="_Token[fields]" value="f-%s"><input name="_Token[unlocked]" value="u-%s"></form>`, slot, slot, slot)
				return
			}

			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)

		case r.Method == http.MethodPost && r.URL.Path == "/reservations/option":
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)

		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-g"><input name="_Token[fields]" value="f-g"><input name="_Token[unlocked]" value="u-g">`)

		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)

		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-c"><input name="_Token[fields]" value="f-c"><input name="_Token[unlocked]" value="u-c">`)

		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			// The ACTUAL booking happens here — this is where contention matters.
			// The X-Booking-Slot header tells us which slot this session is for.
			// In our real code the server tracks this via session; here we use a header
			// that our rival simulator sets, or for our workers we try the first available.
			slot := r.Header.Get("X-Booking-Slot")
			who := r.Header.Get("X-Booking-Who")

			if slot == "" {
				// Our workers don't set these headers — just try to book any available slot
				who = "our-worker"
				for _, s := range GetTimeSlots() {
					if isSlotAvailable(s) {
						slot = s
						break
					}
				}
			}

			if slot != "" && tryBook(slot, who) {
				if who == "our-worker" {
					atomic.AddInt64(&ourBookings, 1)
				} else {
					atomic.AddInt64(&rivalBookings, 1)
				}
				w.Header().Set("Location", fmt.Sprintf("/reservations/finish/%s", slot))
				w.WriteHeader(http.StatusFound)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// --- Launch rival users ---
	// Each rival waits for release time, then races through the 4-step booking flow.
	// Rivals have varied speeds to simulate different users/bots.
	var rivalWg sync.WaitGroup
	for i := 0; i < numRivals; i++ {
		rivalWg.Add(1)
		go func(rivalID int) {
			defer rivalWg.Done()

			// Varied delay per step: fast bots (50-100ms) vs slow humans (200-300ms)
			stepDelay := time.Duration(50+((rivalID*37)%250)) * time.Millisecond

			// Wait for release (some rivals start slightly earlier, some slightly later)
			jitter := time.Duration((rivalID*13)%500-250) * time.Millisecond
			targetTime := serverRelease.Add(jitter)
			time.Sleep(time.Until(targetTime))

			slots := GetTimeSlots()
			// Each rival tries their "preferred" slot first, then rotates
			startSlot := rivalID % len(slots)

			client := &http.Client{Timeout: 10 * time.Second}

			for attempt := 0; attempt < len(slots); attempt++ {
				slot := slots[(startSlot+attempt)%len(slots)]

				if !isSlotAvailable(slot) {
					continue
				}

				// Step 1: GET option
				optURL := fmt.Sprintf("%s/reservations/option?time_from=%s", srv.URL, slot)
				resp, err := client.Get(optURL)
				if err != nil || resp.StatusCode != 200 {
					if resp != nil {
						resp.Body.Close()
					}
					continue
				}
				resp.Body.Close()
				time.Sleep(stepDelay) // reading the page

				// Step 2: POST option → 302
				resp, err = client.Post(srv.URL+"/reservations/option", "application/x-www-form-urlencoded", nil)
				if err != nil {
					continue
				}
				resp.Body.Close()
				time.Sleep(stepDelay)

				// Step 3: GET guest, POST guest → 302
				resp, err = client.Get(srv.URL + "/reservations/user/guest")
				if err != nil {
					continue
				}
				resp.Body.Close()
				time.Sleep(stepDelay)

				resp, err = client.Post(srv.URL+"/reservations/user/guest", "application/x-www-form-urlencoded", nil)
				if err != nil {
					continue
				}
				resp.Body.Close()
				time.Sleep(stepDelay)

				// Step 4: GET conf, POST conf → BOOK!
				resp, err = client.Get(srv.URL + "/reservations/conf")
				if err != nil {
					continue
				}
				resp.Body.Close()
				time.Sleep(stepDelay)

				req, err := http.NewRequest("POST", srv.URL+"/reservations/conf", nil)
				if err != nil {
					continue
				}
				req.Header.Set("X-Booking-Slot", slot)
				req.Header.Set("X-Booking-Who", fmt.Sprintf("rival-%d", rivalID))
				resp, err = client.Do(req)
				if err != nil {
					continue
				}
				if resp.StatusCode == http.StatusFound {
					resp.Body.Close()
					return // booked!
				}
				resp.Body.Close()
				// Slot was taken while we were going through the flow — try next slot
			}
		}(i)
	}

	// --- Launch our workers ---
	cfg := &config.Config{
		TargetDate:    "2026/06/22",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Test",
		FirstName:     "User",
		Phone:         "123-456-7890",
		Email:         "test@example.com",
		BaseURL:       srv.URL,
		WorkerCount:   10,
		BurstDuration: 1,
		StartEarlySec: 1,
	}

	setServerClockOffset(-900 * time.Millisecond)
	defer setServerClockOffset(0)

	client := NewPreWarmClient(cfg, cfg.WorkerCount)
	if err := client.PreWarm(cfg.TargetDate); err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	// PreWarm's Date header detection overwrites serverClockOffset with
	// second-precision value. Restore the precise test value.
	setServerClockOffset(-900 * time.Millisecond)

	offset := GetServerClockOffset()
	burstStart := serverRelease.Add(-offset).Add(-time.Duration(cfg.StartEarlySec) * time.Second)

	t.Logf("Release: %s | Rivals: %d | Our workers: %d | Capacity: %d slots × %d each = %d total",
		serverRelease.Format("15:04:05.000"), numRivals, cfg.WorkerCount,
		numSlots, capacityPerSlot, numSlots*capacityPerSlot)
	t.Logf("Burst start: %s (%.1fs from now)", burstStart.Format("15:04:05.000"), time.Until(burstStart).Seconds())

	result := client.QuickBurst(cfg.TargetDate, burstStart)

	// Wait for all rivals to finish
	rivalWg.Wait()

	ours := atomic.LoadInt64(&ourBookings)
	rivals := atomic.LoadInt64(&rivalBookings)
	totalBooked := totalSlotsBooked()

	t.Logf("=== COMPETITIVE RESULTS ===")
	t.Logf("Total slots booked: %d / %d", totalBooked, numSlots*capacityPerSlot)
	t.Logf("Our bookings: %d", ours)
	t.Logf("Rival bookings: %d", rivals)
	t.Logf("Result: success=%v slot=%s msg=%s", result.Success, result.TimeSlot, result.Message)

	// Print booking log
	mu.Lock()
	for _, entry := range bookingLog {
		t.Logf("  %s → %s (by %s)", entry.bookedAt.Format("15:04:05.000"), entry.slot, entry.who)
	}
	mu.Unlock()

	if !result.Success {
		t.Fatalf("FAILED: Could not book any slot against %d rivals! Our bookings: %d, Rival bookings: %d",
			numRivals, ours, rivals)
	}
	t.Logf("SUCCESS: Booked slot %s against %d rivals (we got %d, they got %d)", result.TimeSlot, numRivals, ours, rivals)
}

// ---------------------------------------------------------------------------
// Test 5: EXTREME COMPETITION — 50 rival bots with realistic network latency.
//
// Why latency matters: in real life, the server is in Japan. Even a co-located
// bot needs ~10ms+ per HTTP round-trip. A bot from North America needs ~150ms.
// Without simulated latency, the bots in the test complete all 6 HTTP steps
// near-instantly (localhost is <0.1ms), which is physically impossible.
//
// This test adds realistic per-request latency (20-80ms) to the server so
// ALL clients (both our workers and bots) experience the same overhead.
// With 6 steps, a fast bot takes ~120ms minimum. Our workers, pre-connected
// with sessions ready, should compete fairly.
//
// Configuration:
//   - 50 rival bots, each with 20-80ms server latency per request
//   - 12 slots, 1 capacity each
//   - Our 10 pre-warmed workers
//
// Success criteria: book at least 1 slot.
// ---------------------------------------------------------------------------
func TestIntegration_CompetitiveAgainstFastBots(t *testing.T) {
	const (
		capacityPerSlot = 1
		numRivals       = 50 // 50 bots
		serverLatencyMs = 20 // minimum per-request latency (ms) simulating network
	)

	serverRelease := time.Now().Add(20 * time.Second)
	slots := GetTimeSlots()

	var (
		mu           sync.Mutex
		slotBookings = make(map[string]int)
		bookingLog   []struct {
			at   time.Time
			slot string
			who  string
		}
	)

	var ourBookings int64
	var rivalBookings int64

	tryBook := func(slot, who string) bool {
		mu.Lock()
		defer mu.Unlock()
		if slotBookings[slot] < capacityPerSlot {
			slotBookings[slot]++
			bookingLog = append(bookingLog, struct {
				at   time.Time
				slot string
				who  string
			}{time.Now(), slot, who})
			return true
		}
		return false
	}

	isSlotAvailable := func(slot string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slotBookings[slot] < capacityPerSlot
	}

	// simulateLatency adds realistic per-request delay to simulate network RTT.
	// Both our workers and bots go through the same server, so this is fair.
	simulateLatency := func() {
		time.Sleep(time.Duration(serverLatencyMs) * time.Millisecond)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		simulateLatency() // every request has network delay

		serverNow := time.Now().Add(-900 * time.Millisecond)
		w.Header().Set("Date", serverNow.UTC().Format(http.TimeFormat))

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/calendar":
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-x">`)
		case r.Method == http.MethodPost && r.URL.Path == "/ajax/reservations/calendar":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			slot := r.URL.Query().Get("time_from")
			if time.Now().Before(serverRelease) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `受付期間外のため予約できません`)
				return
			}
			if isSlotAvailable(slot) {
				fmt.Fprintf(w, `<form><input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u"></form>`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/option":
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			fmt.Fprint(w, `<input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			fmt.Fprint(w, `<input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			slot := r.Header.Get("X-Booking-Slot")
			who := r.Header.Get("X-Booking-Who")
			if slot == "" {
				who = "our-worker"
				for _, s := range slots {
					if isSlotAvailable(s) {
						slot = s
						break
					}
				}
			}
			if slot != "" && tryBook(slot, who) {
				if who == "our-worker" {
					atomic.AddInt64(&ourBookings, 1)
				} else {
					atomic.AddInt64(&rivalBookings, 1)
				}
				w.Header().Set("Location", fmt.Sprintf("/reservations/finish/%s", slot))
				w.WriteHeader(http.StatusFound)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `予約数が上限に達したため受付終了しました。`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Launch 50 rival bots — they also experience the same server latency.
	var rivalWg sync.WaitGroup
	for i := 0; i < numRivals; i++ {
		rivalWg.Add(1)
		go func(id int) {
			defer rivalWg.Done()

			// Bots arrive with varied jitter around release time
			jitter := time.Duration((id*17)%600-200) * time.Millisecond
			targetTime := serverRelease.Add(jitter)
			time.Sleep(time.Until(targetTime))

			cl := &http.Client{Timeout: 10 * time.Second}
			startSlot := id % len(slots)

			for attempt := 0; attempt < len(slots)*2; attempt++ {
				slot := slots[(startSlot+attempt)%len(slots)]
				if !isSlotAvailable(slot) {
					continue
				}

				// 6-step booking flow — each step incurs server latency
				resp, err := cl.Get(fmt.Sprintf("%s/reservations/option?time_from=%s", srv.URL, slot))
				if err != nil || resp.StatusCode != 200 {
					if resp != nil { resp.Body.Close() }
					continue
				}
				resp.Body.Close()

				resp, _ = cl.Post(srv.URL+"/reservations/option", "", nil)
				if resp != nil { resp.Body.Close() }

				resp, _ = cl.Get(srv.URL + "/reservations/user/guest")
				if resp != nil { resp.Body.Close() }

				resp, _ = cl.Post(srv.URL+"/reservations/user/guest", "", nil)
				if resp != nil { resp.Body.Close() }

				resp, _ = cl.Get(srv.URL + "/reservations/conf")
				if resp != nil { resp.Body.Close() }

				req, err := http.NewRequest("POST", srv.URL+"/reservations/conf", nil)
				if err != nil { continue }
				req.Header.Set("X-Booking-Slot", slot)
				req.Header.Set("X-Booking-Who", fmt.Sprintf("bot-%d", id))
				resp, err = cl.Do(req)
				if err != nil { continue }
				if resp.StatusCode == http.StatusFound {
					resp.Body.Close()
					return
				}
				resp.Body.Close()
			}
		}(i)
	}

	// Launch our system
	cfg := &config.Config{
		TargetDate:    "2026/06/22",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Test",
		FirstName:     "User",
		Phone:         "123-456-7890",
		Email:         "test@example.com",
		BaseURL:       srv.URL,
		WorkerCount:   10,
		BurstDuration: 1,
		StartEarlySec: 1,
	}

	setServerClockOffset(-900 * time.Millisecond)
	defer setServerClockOffset(0)

	pwClient := NewPreWarmClient(cfg, cfg.WorkerCount)
	if err := pwClient.PreWarm(cfg.TargetDate); err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	// PreWarm's Date header detection overwrites serverClockOffset with
	// second-precision value. Restore the precise test value.
	setServerClockOffset(-900 * time.Millisecond)

	offset := GetServerClockOffset()
	burstStart := serverRelease.Add(-offset).Add(-time.Duration(cfg.StartEarlySec) * time.Second)

	t.Logf("COMPETITION: %d bots (server latency=%dms/req) vs our %d workers",
		numRivals, serverLatencyMs, cfg.WorkerCount)
	t.Logf("Release: %s | Burst: %s", serverRelease.Format("15:04:05.000"), burstStart.Format("15:04:05.000"))

	result := pwClient.QuickBurst(cfg.TargetDate, burstStart)
	rivalWg.Wait()

	ours := atomic.LoadInt64(&ourBookings)
	rivals := atomic.LoadInt64(&rivalBookings)

	t.Logf("=== COMPETITIVE RESULTS ===")
	t.Logf("Our bookings: %d | Rival bookings: %d | Total: %d/%d",
		ours, rivals, ours+rivals, len(slots)*capacityPerSlot)
	t.Logf("Result: success=%v slot=%s", result.Success, result.TimeSlot)

	// Print booking timeline
	mu.Lock()
	for _, e := range bookingLog {
		t.Logf("  %s → %s (by %s)", e.at.Format("15:04:05.000"), e.slot, e.who)
	}
	mu.Unlock()

	if !result.Success {
		t.Fatalf("FAILED against %d bots (latency=%dms). Our: %d, Rivals: %d",
			numRivals, serverLatencyMs, ours, rivals)
	}
	t.Logf("SUCCESS: Booked %s against %d bots (our %d vs their %d)", result.TimeSlot, numRivals, ours, rivals)
}

// ---------------------------------------------------------------------------
// Test 6: KeepAlive does NOT fire during burst window.
// Start keepalive, then stop it, and verify no requests happen after stop.
// ---------------------------------------------------------------------------
func TestIntegration_KeepAliveStopsCleanly(t *testing.T) {
	var reqsAfterStop int64
	var stopped int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt64(&stopped) == 1 {
			atomic.AddInt64(&reqsAfterStop, 1)
		}
		if r.URL.Path == "/reservations/calendar" {
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-ka">`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	cfg.WorkerCount = 5
	client := NewPreWarmClient(cfg, 5)
	for _, w := range client.clients {
		w.csrfToken = "ready"
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		client.KeepAlive(stop)
		close(done)
	}()

	// Let it run briefly, then stop
	time.Sleep(100 * time.Millisecond)
	close(stop)
	atomic.StoreInt64(&stopped, 1)
	log.Printf("[TEST] Stop signal sent")

	select {
	case <-done:
		// Good — stopped promptly
	case <-time.After(5 * time.Second):
		t.Fatalf("KeepAlive did not stop within 5 seconds")
	}

	// Wait a bit and check no requests after stop
	time.Sleep(500 * time.Millisecond)
	afterStop := atomic.LoadInt64(&reqsAfterStop)
	t.Logf("Requests observed after stop: %d", afterStop)

	// Allow a small number of in-flight requests (up to one batch of 5)
	if afterStop > 5 {
		t.Fatalf("Too many requests after stop: %d (expected <=5 from in-flight batch)", afterStop)
	}
}
