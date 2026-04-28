package booking

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"visa_monitor/internal/config"
)

type AggressiveClient struct {
	cfg     *config.Config
	workers []*aggressiveWorker
	baseURL string
}

type aggressiveWorker struct {
	id     int
	client *http.Client
	ready  atomic.Bool
}

func NewAggressiveClient(cfg *config.Config, numWorkers int) *AggressiveClient {
	// FIX: use domain directly so CookieJar domain matching works correctly.
	// IP-based URLs cause Set-Cookie domain mismatches and sessions are never sent.
	baseURL := cfg.BaseURL

	sharedTransport := buildTransport()

	workers := make([]*aggressiveWorker, numWorkers)
	for i := 0; i < numWorkers; i++ {
		jar, _ := NewThreadSafeJar()
		workers[i] = &aggressiveWorker{
			id: i,
			client: &http.Client{
				Jar:       jar,
				Timeout:   30 * time.Second,
				Transport: sharedTransport,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
	}

	log.Printf("[AGGRESSIVE] Created %d workers → %s", numWorkers, baseURL)
	return &AggressiveClient{
		cfg:     cfg,
		workers: workers,
		baseURL: baseURL,
	}
}

// WarmUp pre-establishes TCP+TLS connections for all workers in staggered batches.
func (a *AggressiveClient) WarmUp() error {
	const batchSize = 5
	const batchDelay = 2 * time.Second
	log.Printf("[WARMUP] Pre-warming %d connections (batch=%d)...", len(a.workers), batchSize)

	var errCount int32

	for i := 0; i < len(a.workers); i += batchSize {
		end := i + batchSize
		if end > len(a.workers) {
			end = len(a.workers)
		}

		var wg sync.WaitGroup
		for _, w := range a.workers[i:end] {
			wg.Add(1)
			go func(w *aggressiveWorker) {
				defer wg.Done()
				req, err := http.NewRequest("GET", a.baseURL+"/reservations/calendar", nil)
				if err != nil {
					log.Printf("[WARMUP] worker=%d create request failed: %v", w.id, err)
					atomic.AddInt32(&errCount, 1)
					return
				}
				req.Header.Set("User-Agent", userAgent)
				resp, err := w.client.Do(req)
				if err != nil {
					atomic.AddInt32(&errCount, 1)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}(w)
		}
		wg.Wait()

		if end < len(a.workers) {
			time.Sleep(batchDelay)
		}
	}

	log.Printf("[WARMUP] Complete. Errors: %d/%d", errCount, len(a.workers))
	return nil
}

// InitAllSessions initialises a session (CSRF token + calendar POST) for every
// worker in staggered batches with retries.
func (a *AggressiveClient) InitAllSessions(date string) error {
	const batchSize = 5
	const batchDelay = 2 * time.Second
	const maxRetries = 3
	const retryDelay = 5 * time.Second

	log.Printf("[INIT] Initialising sessions for %d workers (batch=%d)...", len(a.workers), batchSize)

	var readyCount int32

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			var pending []*aggressiveWorker
			for _, w := range a.workers {
				if !w.ready.Load() {
					pending = append(pending, w)
				}
			}
			if len(pending) == 0 {
				break
			}
			log.Printf("[INIT] Retry %d/%d: %d workers pending, waiting %v...",
				attempt, maxRetries-1, len(pending), retryDelay)
			time.Sleep(retryDelay)
		}

		var pending []*aggressiveWorker
		for _, w := range a.workers {
			if !w.ready.Load() {
				pending = append(pending, w)
			}
		}

		for i := 0; i < len(pending); i += batchSize {
			end := i + batchSize
			if end > len(pending) {
				end = len(pending)
			}
			batch := pending[i:end]

			var wg sync.WaitGroup
			for _, w := range batch {
				wg.Add(1)
				go func(w *aggressiveWorker) {
					defer wg.Done()
					if err := a.initWorkerSession(w, date); err != nil {
						log.Printf("[Worker %d] Session init failed: %v", w.id, err)
						return
					}
					w.ready.Store(true)
					atomic.AddInt32(&readyCount, 1)
				}(w)
			}
			wg.Wait()

			if end < len(pending) {
				time.Sleep(batchDelay)
			}
		}

		log.Printf("[INIT] After attempt %d: %d/%d workers ready", attempt+1, readyCount, len(a.workers))
	}

	log.Printf("[INIT] Complete: %d/%d workers ready", readyCount, len(a.workers))

	if readyCount == 0 {
		return fmt.Errorf("all session inits failed")
	}
	return nil
}

func (a *AggressiveClient) initWorkerSession(w *aggressiveWorker, date string) error {
	// Step 1: get CSRF token
	req, err := http.NewRequest("GET", a.baseURL+"/reservations/calendar", nil)
	if err != nil {
		return fmt.Errorf("create calendar request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read calendar response: %w", err)
	}

	csrfMatch := reCsrfValue.FindStringSubmatch(string(body))
	if len(csrfMatch) < 2 {
		return fmt.Errorf("no CSRF token")
	}

	// Step 2: set session via calendar POST
	formData := url.Values{}
	formData.Set("event", a.cfg.EventID)
	formData.Set("plan", a.cfg.PlanID)
	formData.Set("date", date)
	formData.Set("disp_type", "day")
	formData.Set("search", "exec")
	formData.Set("_csrfToken", csrfMatch[1])

	req2, err := http.NewRequest("POST", a.baseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("create calendar POST request: %w", err)
	}
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req2.Header.Set("X-Requested-With", "XMLHttpRequest")
	req2.Header.Set("User-Agent", userAgent)

	resp2, err := w.client.Do(req2)
	if err != nil {
		return err
	}
	postBody, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		return fmt.Errorf("read calendar POST response: %w", err)
	}
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return fmt.Errorf("calendar POST failed: status=%d body=%q", resp2.StatusCode, shortBody(postBody))
	}
	return nil
}

// BurstBook spawns one goroutine per ready worker, each hammering its assigned
// time slot until a booking succeeds or the 5-minute window expires.
// FIX: each goroutine uses a FIXED worker (not round-robin) so session cookies
// are never mixed between workers.
func (a *AggressiveClient) BurstBook(date string, _ int) *Result {
	slots := GetTimeSlots()
	results := make(chan *Result, 1)
	var stopFlag int32
	var wg sync.WaitGroup

	readyWorkers := 0
	for _, w := range a.workers {
		if !w.ready.Load() {
			continue
		}
		readyWorkers++
	}
	log.Printf("[BURST] Starting with %d ready workers, %d slots", readyWorkers, len(slots))

	for i, w := range a.workers {
		if !w.ready.Load() {
			continue
		}
		slot := slots[i%len(slots)]
		wg.Add(1)
		go func(w *aggressiveWorker, slot string) {
			defer wg.Done()
			a.workerLoop(w, date, slot, results, &stopFlag)
		}(w, slot)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	select {
	case result, ok := <-results:
		atomic.StoreInt32(&stopFlag, 1)
		if ok {
			return result
		}
		return &Result{Success: false, Message: "All workers exited"}
	case <-time.After(5 * time.Minute):
		atomic.StoreInt32(&stopFlag, 1)
		return &Result{Success: false, Message: "Timeout"}
	}
}

func (a *AggressiveClient) workerLoop(w *aggressiveWorker, date, timeSlot string, results chan<- *Result, stopFlag *int32) {
	optionURL := fmt.Sprintf("%s/reservations/option?event_id=%s&event_plan_id=%s&date=%s&time_from=%s",
		a.baseURL, a.cfg.EventID, a.cfg.PlanID, url.QueryEscape(date), url.QueryEscape(timeSlot))

	for {
		if atomic.LoadInt32(stopFlag) == 1 {
			return
		}

		req, err := http.NewRequest("GET", optionURL, nil)
		if err != nil {
			log.Printf("[Worker %d] Create option request failed: %v", w.id, err)
			return
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Referer", a.baseURL+"/reservations/calendar")

		resp, err := w.client.Do(req)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		if resp.StatusCode == 200 {
			// Slot available — option page loaded, submit any option form
			// before proceeding so the selected slot is bound to the session.
			optionBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				log.Printf("[Worker %d] Read option page failed: %v", w.id, readErr)
				time.Sleep(5 * time.Millisecond)
				continue
			}
			guestURL := a.submitOptionPage(w, optionBody)
			result := a.submitBooking(w.client, guestURL, timeSlot)
			if result.Success {
				select {
				case results <- result:
				default:
				}
				return
			}
		} else if resp.StatusCode == 302 {
			location := resp.Header.Get("Location")
			resp.Body.Close()

			if strings.Contains(location, "/guest") || strings.Contains(location, "/user/guest") {
				result := a.submitBooking(w.client, location, timeSlot)
				if result.Success {
					select {
					case results <- result:
					default:
					}
					return
				}
			} else {
				time.Sleep(5 * time.Millisecond)
			}
		} else {
			resp.Body.Close()
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func (a *AggressiveClient) submitOptionPage(w *aggressiveWorker, body []byte) string {
	defaultGuestURL := a.baseURL + "/reservations/user/guest"
	html := string(body)

	csrf, fields, unlocked := extractFormTokens(html)
	if csrf == "" {
		log.Printf("[AGG-OPTIONPAGE] worker=%d no CSRF token on option page (bodyLen=%d), going direct to guest", w.id, len(body))
		return defaultGuestURL
	}

	actionURL := a.baseURL + "/reservations/option"
	if m := reFormAction.FindStringSubmatch(html); len(m) > 1 && m[1] != "" {
		action := m[1]
		switch {
		case strings.HasPrefix(action, "http"):
			actionURL = action
		case strings.HasPrefix(action, "/"):
			actionURL = a.baseURL + action
		default:
			actionURL = a.baseURL + "/" + action
		}
	}

	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", csrf)
	formData.Set("_Token[fields]", fields)
	formData.Set("_Token[unlocked]", unlocked)

	req, err := http.NewRequest("POST", actionURL, strings.NewReader(formData.Encode()))
	if err != nil {
		log.Printf("[AGG-OPTIONPAGE] worker=%d create option POST failed: %v", w.id, err)
		return defaultGuestURL
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", a.baseURL)
	req.Header.Set("Referer", actionURL)

	start := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		log.Printf("[AGG-OPTIONPAGE] worker=%d option POST error: %v", w.id, err)
		return defaultGuestURL
	}

	location := ""
	if resp.StatusCode == 302 {
		location = resp.Header.Get("Location")
	}
	drainBody(resp)
	log.Printf("[AGG-OPTIONPAGE] worker=%d option POST status=%d location=%q latency=%v",
		w.id, resp.StatusCode, location, time.Since(start).Round(time.Millisecond))

	if location != "" {
		if !strings.HasPrefix(location, "http") {
			return a.baseURL + location
		}
		return location
	}
	return defaultGuestURL
}

func (a *AggressiveClient) submitBooking(client *http.Client, guestURL, timeSlot string) *Result {
	if !strings.HasPrefix(guestURL, "http") {
		guestURL = a.baseURL + guestURL
	}

	req, err := http.NewRequest("GET", guestURL, nil)
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Create guest request failed: %v", err)}
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return &Result{Success: false, Message: "Guest page failed"}
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Read guest response failed: %v", err)}
	}

	tokenCsrf, tokenFields, tokenUnlocked := extractFormTokens(string(body))
	if tokenCsrf == "" || tokenFields == "" {
		return &Result{Success: false, Message: "No form tokens on guest page"}
	}

	// FIX: use SplitPhone from config instead of hard-coded digits
	parts := SplitPhone(a.cfg.Phone)

	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", tokenCsrf)
	formData.Set("users[addition_values][4][0]", a.cfg.FamilyName)
	formData.Set("users[addition_values][4][1]", a.cfg.FirstName)
	formData.Set("users[addition_values][6][0]", parts[0])
	formData.Set("users[addition_values][6][1]", parts[1])
	formData.Set("users[addition_values][6][2]", parts[2])
	formData.Set("users[addition_values][16]", "")
	formData.Set("users[addition_values][17]", "")
	formData.Set("users[mail]", a.cfg.Email)
	formData.Set("users[mail_confirm]", a.cfg.Email)
	formData.Set("_Token[fields]", tokenFields)
	formData.Set("_Token[unlocked]", tokenUnlocked)

	req2, err := http.NewRequest("POST", guestURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Create submit request failed: %v", err)}
	}
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", userAgent)
	req2.Header.Set("Origin", a.baseURL)
	req2.Header.Set("Referer", guestURL)

	resp2, err := client.Do(req2)
	if err != nil {
		return &Result{Success: false, Message: "Submit failed"}
	}
	resp2.Body.Close()

	if resp2.StatusCode == 302 {
		return a.confirmBooking(client, timeSlot)
	}
	return &Result{Success: false, Message: fmt.Sprintf("Submit status: %d", resp2.StatusCode)}
}

func (a *AggressiveClient) confirmBooking(client *http.Client, timeSlot string) *Result {
	req, err := http.NewRequest("GET", a.baseURL+"/reservations/conf", nil)
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Create conf request failed: %v", err)}
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return &Result{Success: false, Message: "Conf page failed"}
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Read conf response failed: %v", err)}
	}

	tokenCsrf, tokenFields, tokenUnlocked := extractFormTokens(string(body))
	if tokenCsrf == "" || tokenFields == "" {
		return &Result{Success: false, Message: "No form tokens on conf page"}
	}

	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", tokenCsrf)
	formData.Set("_Token[fields]", tokenFields)
	formData.Set("_Token[unlocked]", tokenUnlocked)

	req2, err := http.NewRequest("POST", a.baseURL+"/reservations/conf", strings.NewReader(formData.Encode()))
	if err != nil {
		return &Result{Success: false, Message: fmt.Sprintf("Create confirm request failed: %v", err)}
	}
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", userAgent)
	req2.Header.Set("Origin", a.baseURL)
	req2.Header.Set("Referer", a.baseURL+"/reservations/conf")

	resp2, err := client.Do(req2)
	if err != nil {
		return &Result{Success: false, Message: "Confirm failed"}
	}
	resp2.Body.Close()

	if resp2.StatusCode == 302 {
		location := resp2.Header.Get("Location")
		if strings.Contains(location, "/finish/") {
			return &Result{Success: true, TimeSlot: timeSlot, Message: "Booked! " + location}
		}
	}
	return &Result{Success: false, Message: fmt.Sprintf("Confirm status: %d", resp2.StatusCode)}
}

// ThreadSafeJar is a simple thread-safe cookie jar.
type ThreadSafeJar struct {
	mu    sync.RWMutex
	store map[string][]*http.Cookie
}

func NewThreadSafeJar() (*ThreadSafeJar, error) {
	return &ThreadSafeJar{store: make(map[string][]*http.Cookie)}, nil
}

func (j *ThreadSafeJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	existing := j.store[u.Host]
	for _, c := range cookies {
		replaced := false
		for i, e := range existing {
			if e.Name == c.Name {
				existing[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			existing = append(existing, c)
		}
	}
	j.store[u.Host] = existing
}

func (j *ThreadSafeJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.store[u.Host]
}
