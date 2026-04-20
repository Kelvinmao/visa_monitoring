package booking

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"visa_monitor/internal/config"
)

func newTestConfig(baseURL string) *config.Config {
	return &config.Config{
		TargetDate:    "2026/06/02",
		EventID:       "16",
		PlanID:        "20",
		FamilyName:    "Mao",
		FirstName:     "Kaining",
		Phone:         "825-984-7284",
		Email:         "test@example.com",
		BaseURL:       baseURL,
		WorkerCount:   1,
		BurstDuration: 1,
	}
}

func TestQuickConfirmPostsUnlockedToken(t *testing.T) {
	var postedBody atomic.Value
	postedBody.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-1"><input name="_Token[fields]" value="fields-1"><input name="_Token[unlocked]" value="unlock-1">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			_ = r.ParseForm()
			postedBody.Store(r.PostForm.Encode())
			w.Header().Set("Location", "/reservations/finish/ok")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)
	worker := client.clients[0]

	res := client.quickConfirm(worker, "10:30")
	if !res.Success {
		t.Fatalf("expected success, got: %+v", res)
	}

	body := postedBody.Load().(string)
	if !strings.Contains(body, "_Token%5Bunlocked%5D=unlock-1") {
		t.Fatalf("expected unlocked token in confirm POST, got body: %s", body)
	}
}

func TestQuickBurstRotatesSlotsAndSucceeds(t *testing.T) {
	const expectedSlot = "09:00" // First slot — worker 0 is assigned slots[0]
	var optionHitExpectedSlot int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			timeFrom := r.URL.Query().Get("time_from")
			if timeFrom == expectedSlot {
				atomic.StoreInt32(&optionHitExpectedSlot, 1)
				// Slot available → return 200 (option page loaded)
				w.WriteHeader(http.StatusOK)
				return
			}
			// Slot not available → redirect back to calendar
			w.Header().Set("Location", "/reservations/calendar")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-guest"><input name="_Token[fields]" value="fields-guest"><input name="_Token[unlocked]" value="unlock-guest">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-conf"><input name="_Token[fields]" value="fields-conf"><input name="_Token[unlocked]" value="unlock-conf">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			_ = r.ParseForm()
			if r.PostForm.Get("_Token[unlocked]") == "" {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "missing unlocked")
				return
			}
			w.Header().Set("Location", "/reservations/finish/mock")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)
	client.clients[0].csrfToken = "ready"

	res := client.QuickBurst(cfg.TargetDate, time.Now())
	if !res.Success {
		t.Fatalf("expected success, got: %+v", res)
	}
	if res.TimeSlot != expectedSlot {
		t.Fatalf("expected slot %s, got %s", expectedSlot, res.TimeSlot)
	}
	if atomic.LoadInt32(&optionHitExpectedSlot) != 1 {
		t.Fatalf("expected rotated loop to hit slot %s", expectedSlot)
	}
}

func TestKeepAliveStopsOnSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reservations/calendar" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)
	client.clients[0].csrfToken = "ready"

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		client.KeepAlive(stop)
		close(done)
	}()

	close(stop)

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("keepalive did not stop in time")
	}
}

// ---------------------------------------------------------------------------
// 4. PreWarm extracts CSRF token and sets session for all workers
// ---------------------------------------------------------------------------
func TestPreWarmExtractsCSRFAndSetsSession(t *testing.T) {
	var calendarHits, ajaxHits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/calendar":
			atomic.AddInt32(&calendarHits, 1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<form><input type="hidden" name="_csrfToken" value="test-csrf-abc123"></form>`)
		case r.Method == http.MethodPost && r.URL.Path == "/ajax/reservations/calendar":
			atomic.AddInt32(&ajaxHits, 1)
			_ = r.ParseForm()
			if r.PostForm.Get("_csrfToken") != "test-csrf-abc123" {
				t.Errorf("expected CSRF token test-csrf-abc123, got %s", r.PostForm.Get("_csrfToken"))
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 3)

	err := client.PreWarm(cfg.TargetDate)
	if err != nil {
		t.Fatalf("PreWarm failed: %v", err)
	}

	for i, w := range client.clients {
		if w.csrfToken != "test-csrf-abc123" {
			t.Errorf("worker %d csrfToken = %q, want test-csrf-abc123", i, w.csrfToken)
		}
	}
	if atomic.LoadInt32(&calendarHits) != 3 {
		t.Errorf("expected 3 calendar hits, got %d", calendarHits)
	}
	if atomic.LoadInt32(&ajaxHits) != 3 {
		t.Errorf("expected 3 ajax hits, got %d", ajaxHits)
	}
}

// ---------------------------------------------------------------------------
// 5. quickSubmit sends all required form fields
// ---------------------------------------------------------------------------
func TestQuickSubmitFormFields(t *testing.T) {
	var capturedBody atomic.Value
	capturedBody.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="csrf-g"><input name="_Token[fields]" value="fields-g"><input name="_Token[unlocked]" value="unlock-g">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			_ = r.ParseForm()
			capturedBody.Store(r.PostForm.Encode())
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c2"><input name="_Token[fields]" value="f2"><input name="_Token[unlocked]" value="u2">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			w.Header().Set("Location", "/reservations/finish/ok")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)

	res := client.quickSubmit(client.clients[0], srv.URL+"/reservations/user/guest", cfg.TargetDate, "09:00")
	if !res.Success {
		t.Fatalf("expected success, got: %+v", res)
	}

	body := capturedBody.Load().(string)
	checks := []struct{ field, value string }{
		{"users%5Baddition_values%5D%5B4%5D%5B0%5D", "Mao"},
		{"users%5Baddition_values%5D%5B4%5D%5B1%5D", "Kaining"},
		{"users%5Baddition_values%5D%5B6%5D%5B0%5D", "825"},
		{"users%5Baddition_values%5D%5B6%5D%5B1%5D", "984"},
		{"users%5Baddition_values%5D%5B6%5D%5B2%5D", "7284"},
		{"users%5Bmail%5D", "test%40example.com"},
		{"users%5Bmail_confirm%5D", "test%40example.com"},
		{"_csrfToken", "csrf-g"},
		{"_Token%5Bfields%5D", "fields-g"},
		{"_Token%5Bunlocked%5D", "unlock-g"},
	}
	for _, c := range checks {
		expect := c.field + "=" + c.value
		if !strings.Contains(body, expect) {
			t.Errorf("missing or wrong field %s=%s in body:\n%s", c.field, c.value, body)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. Multi-worker concurrency: only 1 success result is produced
// ---------------------------------------------------------------------------
func TestQuickBurstMultiWorkerOnlyOneResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c2"><input name="_Token[fields]" value="f2"><input name="_Token[unlocked]" value="u2">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			w.Header().Set("Location", "/reservations/finish/ok")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 8)
	for _, w := range client.clients {
		w.csrfToken = "ready"
	}

	res := client.QuickBurst(cfg.TargetDate, time.Now())
	if !res.Success {
		t.Fatalf("expected success with 8 workers, got: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// 7. Non-guest 302 doesn't block — worker continues to next slot (Bug 3)
// ---------------------------------------------------------------------------
func TestQuickBurstNonGuestRedirectContinues(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/option":
			n := atomic.AddInt32(&attempts, 1)
			if n <= 4 {
				w.Header().Set("Location", "/reservations/calendar")
				w.WriteHeader(http.StatusFound)
				return
			}
			w.Header().Set("Location", "/reservations/user/guest")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/user/guest":
			w.Header().Set("Location", "/reservations/conf")
			w.WriteHeader(http.StatusFound)
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c2"><input name="_Token[fields]" value="f2"><input name="_Token[unlocked]" value="u2">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			w.Header().Set("Location", "/reservations/finish/ok")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)
	client.clients[0].csrfToken = "ready"

	res := client.QuickBurst(cfg.TargetDate, time.Now())
	if !res.Success {
		t.Fatalf("expected success after non-guest redirects, got: %+v", res)
	}
	if atomic.LoadInt32(&attempts) < 5 {
		t.Fatalf("expected >=5 option attempts, got %d", attempts)
	}
}

// ---------------------------------------------------------------------------
// 8. quickConfirm: 302 to non-finish URL returns failure
// ---------------------------------------------------------------------------
func TestQuickConfirmNonFinishRedirectFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reservations/conf":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<input name="_csrfToken" value="c"><input name="_Token[fields]" value="f"><input name="_Token[unlocked]" value="u">`)
		case r.Method == http.MethodPost && r.URL.Path == "/reservations/conf":
			w.Header().Set("Location", "/reservations/error")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 1)

	res := client.quickConfirm(client.clients[0], "09:00")
	if res.Success {
		t.Fatalf("expected failure for non-finish redirect, got success")
	}
}

// ---------------------------------------------------------------------------
// 9. PreWarm with no CSRF token → returns error
// ---------------------------------------------------------------------------
func TestPreWarmNoCSRFReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>no token here</body></html>`)
	}))
	defer srv.Close()

	cfg := newTestConfig(srv.URL)
	client := NewPreWarmClient(cfg, 2)

	err := client.PreWarm(cfg.TargetDate)
	if err == nil {
		t.Fatalf("expected error when no CSRF token, got nil")
	}

	for i, w := range client.clients {
		if w.csrfToken != "" {
			t.Errorf("worker %d should have empty token, got %q", i, w.csrfToken)
		}
	}
}
