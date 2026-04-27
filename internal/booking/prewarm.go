package booking

import (
	"context"
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

// drainBody reads and discards the response body so the underlying TCP
// connection can be reused by the transport's connection pool.
func drainBody(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func shortBody(body []byte) string {
	const max = 400
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max])
}

type PreWarmClient struct {
	cfg     *config.Config
	clients []*PreWarmedWorker
	baseURL string
}

type PreWarmedWorker struct {
	id        int
	client    *http.Client
	csrfToken string
	jar       *ThreadSafeJar

	// Pre-fetched guest page tokens (filled during prewarm).
	// If available, we skip GET guest and POST directly after option 302.
	guestCsrf     string
	guestFields   string
	guestUnlocked string
	guestBody     string // pre-encoded POST body (ready to send)

	// Pre-fetched conf page tokens (filled during prewarm).
	// If available, we skip GET conf and POST directly after guest 302.
	confCsrf     string
	confFields   string
	confUnlocked string
}

// serverClockOffset is the estimated difference: server_time - local_time.
// Positive means server is ahead; we should start earlier.
var serverClockOffset time.Duration

// offsetSamples collects multiple Date header readings for statistical precision.
// The HTTP Date header has only 1-second resolution, so a single sample has up to
// ±1s error. Taking the MAXIMUM across many samples minimizes this bias.
var offsetSamples []time.Duration
var offsetMu sync.Mutex

// recordOffsetSample records one server-clock-offset sample from an HTTP Date header.
func recordOffsetSample(dateStr string) {
	if dateStr == "" {
		return
	}
	serverTime, err := http.ParseTime(dateStr)
	if err != nil {
		return
	}
	sample := serverTime.Sub(time.Now())
	offsetMu.Lock()
	offsetSamples = append(offsetSamples, sample)
	offsetMu.Unlock()
}

// finalizeOffset computes the best offset estimate from all collected samples.
// Because Date headers truncate to seconds, each sample underestimates the true offset
// by a random amount in [0, 1s). The maximum sample is closest to reality.
// We add +500ms as correction for the expected remaining error (~1/(2*N) seconds).
func finalizeOffset() {
	offsetMu.Lock()
	defer offsetMu.Unlock()
	if len(offsetSamples) == 0 {
		return
	}
	maxOffset := offsetSamples[0]
	for _, s := range offsetSamples[1:] {
		if s > maxOffset {
			maxOffset = s
		}
	}
	// The max sample has expected error of ~1/N seconds (N=sample count).
	// Add half of that as a small correction.
	correction := time.Duration(float64(time.Second) / float64(2*len(offsetSamples)))
	serverClockOffset = maxOffset + correction
	log.Printf("[CLOCK] Finalized offset: %v (from %d samples, max=%v, correction=%v)",
		serverClockOffset.Round(time.Millisecond), len(offsetSamples),
		maxOffset.Round(time.Millisecond), correction.Round(time.Millisecond))
}

// GetServerClockOffset returns the detected server-local clock difference.
func GetServerClockOffset() time.Duration {
	return serverClockOffset
}

// CalibrateServerClock fires N rapid HTTP HEAD requests to collect Date header
// samples and refines the server clock offset. Call this after prewarm and
// shortly before burst for maximum accuracy.
func CalibrateServerClock(baseURL string, n int) {
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < n; i++ {
		resp, err := client.Head(baseURL + "/reservations/calendar")
		if err != nil {
			continue
		}
		recordOffsetSample(resp.Header.Get("Date"))
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond) // slight gap to capture different Date seconds
	}
	finalizeOffset()
}

func NewPreWarmClient(cfg *config.Config, numWorkers int) *PreWarmClient {
	baseURL := cfg.BaseURL

	// Share a single transport across all workers for TCP connection pooling.
	// Each worker has its own cookie jar so sessions stay isolated.
	sharedTransport := buildTransport()

	clients := make([]*PreWarmedWorker, numWorkers)
	for i := 0; i < numWorkers; i++ {
		jar, _ := NewThreadSafeJar()
		clients[i] = &PreWarmedWorker{
			id: i,
			client: &http.Client{
				Jar:       jar,
				Timeout:   10 * time.Second,
				Transport: sharedTransport,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
			jar: jar,
		}
	}

	log.Printf("[PREWARM] Created %d workers (shared transport) → %s", numWorkers, baseURL)
	return &PreWarmClient{
		cfg:     cfg,
		clients: clients,
		baseURL: baseURL,
	}
}

// prewarmOneWorker runs the two-step prewarm for a single worker.
// Returns true on success.
func (p *PreWarmClient) prewarmOneWorker(worker *PreWarmedWorker, date string) bool {
	// Step 1: get CSRF token
	req, _ := http.NewRequest("GET", p.baseURL+"/reservations/calendar", nil)
	req.Header.Set("User-Agent", userAgent)

	resp, err := worker.client.Do(req)
	if err != nil {
		log.Printf("[Worker %d] Step 1 failed: %v", worker.id, err)
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Collect server clock sample from Date header (every worker contributes)
	recordOffsetSample(resp.Header.Get("Date"))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[Worker %d] Calendar GET failed: status=%d bodyLen=%d", worker.id, resp.StatusCode, len(body))
		return false
	}

	matches := reCsrfValue.FindStringSubmatch(string(body))
	if len(matches) < 2 {
		log.Printf("[Worker %d] No CSRF token (status=%d bodyLen=%d)", worker.id, resp.StatusCode, len(body))
		return false
	}
	worker.csrfToken = matches[1]

	// Step 2: set session
	formData := url.Values{}
	formData.Set("event", p.cfg.EventID)
	formData.Set("plan", p.cfg.PlanID)
	formData.Set("date", date)
	formData.Set("disp_type", "day")
	formData.Set("search", "exec")
	formData.Set("_csrfToken", worker.csrfToken)

	req2, _ := http.NewRequest("POST", p.baseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req2.Header.Set("X-Requested-With", "XMLHttpRequest")
	req2.Header.Set("User-Agent", userAgent)

	resp2, err := worker.client.Do(req2)
	if err != nil {
		log.Printf("[Worker %d] Step 2 failed: %v", worker.id, err)
		return false
	}
	ajaxBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if worker.id == 0 {
		log.Printf("[Worker 0] PreWarm session response: status=%d bodyLen=%d body=%q",
			resp2.StatusCode, len(ajaxBody), shortBody(ajaxBody))
	}

	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		log.Printf("[Worker %d] Calendar POST failed: status=%d bodyLen=%d body=%q",
			worker.id, resp2.StatusCode, len(ajaxBody), shortBody(ajaxBody))
		return false
	}

	// Step 3 (optional): pre-fetch guest page tokens.
	// If the server allows direct access to the guest form before going through
	// the option step, we cache the tokens and pre-build the POST body.
	// During burst this lets us skip the GET guest round-trip (~1.6s).
	// If the page isn't accessible, we silently fall back to fetching during burst.
	p.prefetchGuestTokens(worker)

	return true
}

// prefetchGuestTokens tries to GET the guest page and cache the form tokens.
// Many CakePHP apps allow direct page access regardless of session flow state.
// If it works, quickSubmit uses the cached body (saves one full RTT).
// If it fails (redirect, error, no tokens), the worker falls back to the
// normal GET-then-POST flow during burst.
func (p *PreWarmClient) prefetchGuestTokens(worker *PreWarmedWorker) {
	guestURL := p.baseURL + "/reservations/user/guest"
	req, _ := http.NewRequest("GET", guestURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", p.baseURL+"/reservations/calendar")

	resp, err := worker.client.Do(req)
	if err != nil {
		if worker.id == 0 {
			log.Printf("[PREFETCH] worker=%d guest GET error: %v", worker.id, err)
		}
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		if worker.id == 0 {
			log.Printf("[PREFETCH] worker=%d guest page status=%d (not cached)", worker.id, resp.StatusCode)
		}
		return
	}

	csrf, fields, unlocked := extractFormTokens(string(body))
	if csrf == "" || fields == "" {
		if worker.id == 0 {
			log.Printf("[PREFETCH] worker=%d no tokens on guest page (bodyLen=%d)", worker.id, len(body))
		}
		return
	}

	parts := SplitPhone(p.cfg.Phone)
	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", csrf)
	formData.Set("users[addition_values][4][0]", p.cfg.FamilyName)
	formData.Set("users[addition_values][4][1]", p.cfg.FirstName)
	formData.Set("users[addition_values][6][0]", parts[0])
	formData.Set("users[addition_values][6][1]", parts[1])
	formData.Set("users[addition_values][6][2]", parts[2])
	formData.Set("users[mail]", p.cfg.Email)
	formData.Set("users[mail_confirm]", p.cfg.Email)
	formData.Set("users[addition_values][16]", "")
	formData.Set("users[addition_values][17]", "")
	formData.Set("_Token[fields]", fields)
	formData.Set("_Token[unlocked]", unlocked)

	worker.guestCsrf = csrf
	worker.guestFields = fields
	worker.guestUnlocked = unlocked
	worker.guestBody = formData.Encode()

	if worker.id == 0 {
		log.Printf("[PREFETCH] worker=%d ✓ cached guest tokens (csrf=%s… fields=%d bytes)",
			worker.id, csrf[:min(8, len(csrf))], len(fields))
	}

	// Also try to pre-fetch conf page tokens
	p.prefetchConfTokens(worker)
}

// prefetchConfTokens tries to GET the conf page and cache the form tokens.
// If successful, quickConfirm can skip GET conf and POST directly.
func (p *PreWarmClient) prefetchConfTokens(worker *PreWarmedWorker) {
	confURL := p.baseURL + "/reservations/conf"
	req, _ := http.NewRequest("GET", confURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", p.baseURL+"/reservations/user/guest")

	resp, err := worker.client.Do(req)
	if err != nil {
		if worker.id == 0 {
			log.Printf("[PREFETCH-CONF] worker=%d error: %v", worker.id, err)
		}
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		if worker.id == 0 {
			log.Printf("[PREFETCH-CONF] worker=%d status=%d (not cached)", worker.id, resp.StatusCode)
		}
		return
	}

	csrf, fields, unlocked := extractFormTokens(string(body))
	if csrf == "" || fields == "" {
		if worker.id == 0 {
			log.Printf("[PREFETCH-CONF] worker=%d no tokens on conf page", worker.id)
		}
		return
	}

	worker.confCsrf = csrf
	worker.confFields = fields
	worker.confUnlocked = unlocked

	if worker.id == 0 {
		log.Printf("[PREFETCH-CONF] worker=%d ✓ cached conf tokens (csrf=%s… fields=%d bytes)",
			worker.id, csrf[:min(8, len(csrf))], len(fields))
	}
}

// PreWarm warms up every worker sequentially (2 at a time) with retries.
// The target server is slow (~3s per request), so we avoid large batches
// that would cause timeouts under load.
func (p *PreWarmClient) PreWarm(date string) error {
	const batchSize = 2
	const batchDelay = 3 * time.Second
	const maxRetries = 5
	const retryDelay = 10 * time.Second

	log.Printf("[PREWARM] Warming up %d workers (batch=%d, delay=%v)...", len(p.clients), batchSize, batchDelay)

	var successCount int32

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			var pending []*PreWarmedWorker
			for _, w := range p.clients {
				if w.csrfToken == "" {
					pending = append(pending, w)
				}
			}
			if len(pending) == 0 {
				break
			}
			log.Printf("[PREWARM] Retry %d/%d: %d workers still pending, waiting %v...",
				attempt, maxRetries-1, len(pending), retryDelay)
			time.Sleep(retryDelay)
		}

		var pending []*PreWarmedWorker
		for _, w := range p.clients {
			if w.csrfToken == "" {
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
			for _, worker := range batch {
				wg.Add(1)
				go func(w *PreWarmedWorker) {
					defer wg.Done()
					if p.prewarmOneWorker(w, date) {
						atomic.AddInt32(&successCount, 1)
					}
				}(worker)
			}
			wg.Wait()

			if end < len(pending) {
				time.Sleep(batchDelay)
			}
		}

		log.Printf("[PREWARM] After attempt %d: %d/%d workers ready", attempt+1, successCount, len(p.clients))
	}

	log.Printf("[PREWARM] Complete: %d/%d workers ready", successCount, len(p.clients))

	// Finalize offset from all collected Date header samples.
	finalizeOffset()

	if successCount == 0 {
		return fmt.Errorf("no workers ready")
	}
	return nil
}

// firstReqProfile captures per-worker timing for profiling.
type firstReqProfile struct {
	goroutineSpawn time.Time
	busyWaitExit   time.Time
	doStart        time.Time
	doEnd          time.Time
}

// QuickBurst fires all pre-warmed workers simultaneously.
// Workers are assigned dedicated slot(s) so each slot gets maximum polling frequency.
// burstStart: exact time when workers should start firing (goroutines sleep then busy-wait).
func (p *PreWarmClient) QuickBurst(date string, burstStart time.Time) *Result {
	slots := GetTimeSlots()
	results := make(chan *Result, 1)
	var stopFlag int32
	var wg sync.WaitGroup

	var activeCount int32
	for _, w := range p.clients {
		if w.csrfToken != "" {
			activeCount++
		}
	}
	log.Printf("[QUICKBURST] Starting with %d active workers, %d slots, burstStart=%s, burstDuration=%dm",
		activeCount, len(slots), burstStart.Format("15:04:05.000"), p.cfg.BurstDuration)
	log.Printf("[QUICKBURST] All workers fire at burstStart (rapid-retry mode, no stagger)")
	log.Printf("[QUICKBURST] Time until burst: %v", time.Until(burstStart).Round(time.Millisecond))

	var requestCount int64
	var lastLogNano int64
	var status200 int64
	var status302 int64
	var status400 int64
	var statusOther int64
	var statusErr int64

	// Track full-rotation failures for early exit.
	type slotState struct {
		capacityFull int64
	}
	slotStates := make([]slotState, len(slots))
	var slotMu sync.Mutex
	fullRotationsWithoutHit := int64(0)
	const minPerSlotForExit = 20 // require >=20 capacity-full per slot before considering exit

	// Pre-construct all option URLs to avoid fmt.Sprintf during burst
	type prebuiltSlot struct {
		slotName  string
		optionURL string
	}
	prebuiltSlots := make([]prebuiltSlot, len(slots))
	for i, slot := range slots {
		prebuiltSlots[i] = prebuiltSlot{
			slotName: slot,
			optionURL: fmt.Sprintf("%s/reservations/option?event_id=%s&event_plan_id=%s&date=%s&time_from=%s",
				p.baseURL, p.cfg.EventID, p.cfg.PlanID, url.QueryEscape(date), url.QueryEscape(slot)),
		}
	}

	firstProfiles := make([]firstReqProfile, len(p.clients))

	quickBurstStart := time.Now()
	log.Printf("[PROFILE] QuickBurst called at %s", quickBurstStart.Format("15:04:05.000000"))

	workerIdx := 0
	for _, worker := range p.clients {
		if worker.csrfToken == "" {
			continue
		}
		startIdx := workerIdx % len(slots)
		seqIdx := workerIdx
		workerIdx++
		wg.Add(1)
		go func(w *PreWarmedWorker, slotIdx int, seqIdx int) {
			defer wg.Done()
			currentIdx := slotIdx

			firstProfiles[w.id].goroutineSpawn = time.Now()

			// Pre-construct request object to minimize work after burst
			pbSlot := prebuiltSlots[currentIdx%len(slots)]
			req, _ := http.NewRequest("GET", pbSlot.optionURL, nil)
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Referer", p.baseURL+"/reservations/calendar")

			myStart := burstStart
			waitDur := time.Until(myStart)
			if waitDur > 500*time.Microsecond {
				time.Sleep(waitDur - 500*time.Microsecond)
			}
			for time.Now().Before(myStart) {
				// Final sub-millisecond busy-wait for precision
			}

			firstProfiles[w.id].busyWaitExit = time.Now()
			if firstProfiles[w.id].doStart.IsZero() {
				firstProfiles[w.id].doStart = time.Now()
			}

			for {
				if atomic.LoadInt32(&stopFlag) == 1 {
					return
				}

				pbSlot = prebuiltSlots[currentIdx%len(slots)]
				req.URL, _ = url.Parse(pbSlot.optionURL)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				req = req.WithContext(ctx)

				resp, err := w.client.Do(req)

				if firstProfiles[w.id].doStart.IsZero() == false && firstProfiles[w.id].doEnd.IsZero() {
					firstProfiles[w.id].doEnd = time.Now()
				}

				count := atomic.AddInt64(&requestCount, 1)
				now := time.Now().UnixNano()
				prev := atomic.LoadInt64(&lastLogNano)
				if now-prev > int64(time.Second) && atomic.CompareAndSwapInt64(&lastLogNano, prev, now) {
					s200 := atomic.LoadInt64(&status200)
					s302 := atomic.LoadInt64(&status302)
					s400 := atomic.LoadInt64(&status400)
					sOther := atomic.LoadInt64(&statusOther)
					sErr := atomic.LoadInt64(&statusErr)
					elapsed := time.Since(burstStart).Round(time.Millisecond)
					log.Printf("[QUICKBURST] t+%v Requests: %d | 200:%d 302:%d 400:%d err:%d other:%d",
						elapsed, count, s200, s302, s400, sErr, sOther)
				}

				if err != nil {
					atomic.AddInt64(&statusErr, 1)
					cancel()
					continue
				}

				mySlot := pbSlot.slotName

				switch {
				case resp.StatusCode == 200:
					atomic.AddInt64(&status200, 1)
				case resp.StatusCode == 302:
					atomic.AddInt64(&status302, 1)
				case resp.StatusCode >= 400 && resp.StatusCode < 500:
					atomic.AddInt64(&status400, 1)
				default:
					atomic.AddInt64(&statusOther, 1)
				}

				if resp.StatusCode == 200 {
					optBody, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					cancel()
					guestURL := p.submitOptionPage(w, optBody)
					log.Printf("[QUICKBURST] worker=%d GOT 200 for slot=%s → submitting", w.id, mySlot)
					result := p.quickSubmit(w, guestURL, date, mySlot)
					if result.Success {
						select {
						case results <- result:
						default:
						}
						atomic.StoreInt32(&stopFlag, 1)
						return
					}
					currentIdx++
					// Update prebuilt slot for next iteration
					pbSlot = prebuiltSlots[currentIdx%len(slots)]
					req.URL, _ = url.Parse(pbSlot.optionURL)
				} else if resp.StatusCode == 302 {
					location := resp.Header.Get("Location")
					drainBody(resp)
					cancel()

					if strings.Contains(location, "guest") {
						log.Printf("[QUICKBURST] worker=%d GOT 302→guest for slot=%s → submitting", w.id, mySlot)
						result := p.quickSubmit(w, location, date, mySlot)
						if result.Success {
							select {
							case results <- result:
							default:
							}
							atomic.StoreInt32(&stopFlag, 1)
							return
						}
						currentIdx++
						pbSlot = prebuiltSlots[currentIdx%len(slots)]
						req.URL, _ = url.Parse(pbSlot.optionURL)
					} else {
						currentIdx++
						pbSlot = prebuiltSlots[currentIdx%len(slots)]
						req.URL, _ = url.Parse(pbSlot.optionURL)
					}
				} else {
					diagBuf := make([]byte, 512)
					n, _ := io.ReadFull(resp.Body, diagBuf)
					resp.Body.Close()
					cancel()
					bodyStr := string(diagBuf[:n])

					if count <= 3 || count%50 == 0 {
						log.Printf("[DIAG] worker=%d slot=%s status=%d body=%q", w.id, mySlot, resp.StatusCode, shortBody([]byte(bodyStr)))
					}

					if strings.Contains(bodyStr, "上限に達した") {
						slotMu.Lock()
						slotStates[currentIdx%len(slots)].capacityFull++
						allFull := true
						for _, ss := range slotStates {
							if ss.capacityFull < minPerSlotForExit {
								allFull = false
								break
							}
						}
						if allFull {
							fullRotationsWithoutHit++
							if fullRotationsWithoutHit == 1 {
								log.Printf("[QUICKBURST] All %d slots capacity-full (after %d reqs). Will keep retrying for full burst duration.",
									len(slots), count)
							}
							if fullRotationsWithoutHit%50 == 0 {
								log.Printf("[QUICKBURST] Still all slots full, rotation %d (%d reqs)",
									fullRotationsWithoutHit, count)
							}
						}
						slotMu.Unlock()
						currentIdx++
						pbSlot = prebuiltSlots[currentIdx%len(slots)]
						req.URL, _ = url.Parse(pbSlot.optionURL)
						continue
					}

					if strings.Contains(bodyStr, "予約できません") || strings.Contains(bodyStr, "受付期間外") {
						continue
					}

					currentIdx++
					pbSlot = prebuiltSlots[currentIdx%len(slots)]
					req.URL, _ = url.Parse(pbSlot.optionURL)
				}
			}
		}(worker, startIdx, seqIdx)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Deadline = burstStart + BurstDuration. Use absolute deadline instead of
	// relative time.After to avoid timer drift from CPU contention.
	burstDuration := time.Duration(p.cfg.BurstDuration) * time.Minute
	if burstDuration <= 0 {
		burstDuration = 10 * time.Minute
		log.Printf("[QUICKBURST] WARNING: BurstDuration<=0, defaulting to %v", burstDuration)
	}
	deadline := burstStart.Add(burstDuration)
	totalTimeout := time.Until(deadline)
	if totalTimeout <= 0 {
		totalTimeout = burstDuration
	}
	log.Printf("[QUICKBURST] Timeout set: deadline=%s, totalTimeout=%v (burstStart=%s + duration=%v)",
		deadline.Format("15:04:05.000"), totalTimeout.Round(time.Millisecond),
		burstStart.Format("15:04:05.000"), burstDuration)

	select {
	case result, ok := <-results:
		atomic.StoreInt32(&stopFlag, 1)
		wg.Wait()
		finalReqs := atomic.LoadInt64(&requestCount)
		log.Printf("[QUICKBURST] Finished: totalRequests=%d, elapsed=%v", finalReqs, time.Since(burstStart).Round(time.Millisecond))
		p.printProfile(firstProfiles, burstStart)
		if ok {
			return result
		}
		return &Result{Success: false, Message: fmt.Sprintf("All workers exited after %d requests", finalReqs)}
	case <-time.After(totalTimeout):
		atomic.StoreInt32(&stopFlag, 1)
		wg.Wait()
		finalReqs := atomic.LoadInt64(&requestCount)
		s200 := atomic.LoadInt64(&status200)
		s302 := atomic.LoadInt64(&status302)
		s400 := atomic.LoadInt64(&status400)
		sErr := atomic.LoadInt64(&statusErr)
		log.Printf("[QUICKBURST] TIMEOUT after %v: totalRequests=%d (200:%d 302:%d 400:%d err:%d)",
			time.Since(burstStart).Round(time.Millisecond), finalReqs, s200, s302, s400, sErr)
		p.printProfile(firstProfiles, burstStart)
		return &Result{Success: false, Message: fmt.Sprintf("Timeout after %d requests in %v", finalReqs, burstDuration)}
	}
}

func (p *PreWarmClient) printProfile(profiles []firstReqProfile, burstStart time.Time) {
	log.Printf("[PROFILE] ════════════════════════════════════════")
	log.Printf("[PROFILE] burstStart = %s", burstStart.Format("15:04:05.000000"))

	var totalSpawnDelay, totalBusyDelay, totalDoLatency time.Duration
	var count, firedCount int

	for i, pf := range profiles {
		if pf.goroutineSpawn.IsZero() {
			continue
		}
		count++
		spawnDelay := pf.goroutineSpawn.Sub(burstStart)

		// Skip workers that never exited busy-wait or never fired a request
		if pf.busyWaitExit.IsZero() || pf.doStart.IsZero() {
			if i < 3 {
				log.Printf("[PROFILE] worker=%2d | spawn:+%s | NEVER FIRED (busyExit=%v doStart=%v)",
					i, spawnDelay, !pf.busyWaitExit.IsZero(), !pf.doStart.IsZero())
			}
			continue
		}

		firedCount++
		busyDelay := pf.busyWaitExit.Sub(burstStart)
		totalSpawnDelay += spawnDelay
		totalBusyDelay += busyDelay

		var doLatency time.Duration
		if !pf.doEnd.IsZero() {
			doLatency = pf.doEnd.Sub(pf.doStart)
			totalDoLatency += doLatency
		}

		if firedCount <= 5 || firedCount == count {
			log.Printf("[PROFILE] worker=%2d | spawn:+%s | busy_exit:+%s | first_req_latency:%s",
				i, spawnDelay, busyDelay, doLatency)
		}
	}

	log.Printf("[PROFILE] Total workers: %d spawned, %d actually fired requests", count, firedCount)
	if firedCount > 0 {
		log.Printf("[PROFILE] Avg spawn delay: %s | avg busy exit: %s | avg first RTT: %s",
			totalSpawnDelay/time.Duration(firedCount),
			totalBusyDelay/time.Duration(firedCount),
			totalDoLatency/time.Duration(firedCount))
	} else {
		log.Printf("[PROFILE] WARNING: No workers fired any requests!")
	}
	log.Printf("[PROFILE] ════════════════════════════════════════")
}

// submitOptionPage parses the option page HTML. If a form with CSRF token is
// present, it submits the form and follows the redirect to the guest page.
// Otherwise it returns the default guest URL.
func (p *PreWarmClient) submitOptionPage(worker *PreWarmedWorker, body []byte) string {
	defaultGuestURL := p.baseURL + "/reservations/user/guest"
	html := string(body)

	csrf, fields, unlocked := extractFormTokens(html)
	if csrf == "" {
		log.Printf("[OPTIONPAGE] worker=%d no CSRF token on option page (bodyLen=%d), going direct to guest", worker.id, len(body))
		return defaultGuestURL
	}

	log.Printf("[OPTIONPAGE] worker=%d found option form tokens, submitting option form", worker.id)

	// Find form action URL
	actionURL := p.baseURL + "/reservations/option"
	if m := reFormAction.FindStringSubmatch(html); len(m) > 1 && m[1] != "" {
		action := m[1]
		if !strings.HasPrefix(action, "http") {
			actionURL = p.baseURL + action
		} else {
			actionURL = action
		}
	}

	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", csrf)
	if fields != "" {
		formData.Set("_Token[fields]", fields)
	}
	if unlocked != "" {
		formData.Set("_Token[unlocked]", unlocked)
	}

	req, _ := http.NewRequest("POST", actionURL, strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", p.baseURL)
	req.Header.Set("Referer", actionURL)

	optStart := time.Now()
	resp, err := worker.client.Do(req)
	if err != nil {
		log.Printf("[OPTIONPAGE] worker=%d option POST error: %v", worker.id, err)
		return defaultGuestURL
	}

	location := ""
	if resp.StatusCode == 302 {
		location = resp.Header.Get("Location")
	}
	drainBody(resp)
	log.Printf("[OPTIONPAGE] worker=%d option POST status=%d location=%q latency=%v",
		worker.id, resp.StatusCode, location, time.Since(optStart).Round(time.Millisecond))

	if location != "" {
		if !strings.HasPrefix(location, "http") {
			return p.baseURL + location
		}
		return location
	}
	return defaultGuestURL
}

// buildGuestBody returns the encoded POST body for the guest form.
// If pre-fetched tokens are available, uses them (fast path: skip GET guest).
// Otherwise, fetches the guest page for fresh tokens (slow path: +1 RTT).
func (p *PreWarmClient) buildGuestBody(worker *PreWarmedWorker, guestURL string) string {
	if worker.guestBody != "" {
		log.Printf("[QUICKSUBMIT] worker=%d using pre-fetched tokens (skip GET guest)", worker.id)
		return worker.guestBody
	}

	// Slow path: GET guest page for fresh tokens
	log.Printf("[QUICKSUBMIT] worker=%d GET guest page: %s", worker.id, guestURL)
	guestStart := time.Now()
	req, _ := http.NewRequest("GET", guestURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", p.baseURL+"/reservations/option")

	resp, err := worker.client.Do(req)
	if err != nil {
		log.Printf("[QUICKSUBMIT] worker=%d guest GET error: %v", worker.id, err)
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("[QUICKSUBMIT] worker=%d guest GET status=%d bodyLen=%d latency=%v",
		worker.id, resp.StatusCode, len(body), time.Since(guestStart).Round(time.Millisecond))

	if resp.StatusCode != 200 {
		log.Printf("[QUICKSUBMIT] Guest GET status=%d (expected 200), slot may be taken. Body=%q",
			resp.StatusCode, shortBody(body))
		return ""
	}

	tokenCsrf, tokenFields, tokenUnlocked := extractFormTokens(string(body))
	if tokenCsrf == "" || tokenFields == "" {
		log.Printf("[QUICKSUBMIT] Missing tokens: csrf=%q fields=%q", tokenCsrf, tokenFields)
		return ""
	}

	parts := SplitPhone(p.cfg.Phone)
	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", tokenCsrf)
	formData.Set("users[addition_values][4][0]", p.cfg.FamilyName)
	formData.Set("users[addition_values][4][1]", p.cfg.FirstName)
	formData.Set("users[addition_values][6][0]", parts[0])
	formData.Set("users[addition_values][6][1]", parts[1])
	formData.Set("users[addition_values][6][2]", parts[2])
	formData.Set("users[mail]", p.cfg.Email)
	formData.Set("users[mail_confirm]", p.cfg.Email)
	formData.Set("users[addition_values][16]", "")
	formData.Set("users[addition_values][17]", "")
	formData.Set("_Token[fields]", tokenFields)
	formData.Set("_Token[unlocked]", tokenUnlocked)
	return formData.Encode()
}

func (p *PreWarmClient) quickSubmit(worker *PreWarmedWorker, guestURL, date, timeSlot string) *Result {
	if !strings.HasPrefix(guestURL, "http") {
		guestURL = p.baseURL + guestURL
	}

	encodedBody := p.buildGuestBody(worker, guestURL)
	if encodedBody == "" {
		return &Result{Success: false, Message: "Failed to build guest form body"}
	}

	req2, _ := http.NewRequest("POST", guestURL, strings.NewReader(encodedBody))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", userAgent)
	req2.Header.Set("Origin", p.baseURL)
	req2.Header.Set("Referer", guestURL)

	submitStart := time.Now()
	resp2, err := worker.client.Do(req2)
	if err != nil {
		log.Printf("[QUICKSUBMIT] worker=%d POST guest error: %v", worker.id, err)
		return &Result{Success: false, Message: fmt.Sprintf("Submit failed: %v", err)}
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	log.Printf("[QUICKSUBMIT] worker=%d POST guest status=%d latency=%v",
		worker.id, resp2.StatusCode, time.Since(submitStart).Round(time.Millisecond))

	if resp2.StatusCode == 302 {
		log.Printf("[QUICKSUBMIT] worker=%d → 302 redirect, proceeding to confirm for slot=%s", worker.id, timeSlot)
		return p.quickConfirm(worker, timeSlot)
	}

	// If we used cached tokens and they were rejected, retry with fresh GET.
	if worker.guestBody != "" {
		log.Printf("[QUICKSUBMIT] worker=%d cached tokens rejected (status=%d), retrying with fresh GET",
			worker.id, resp2.StatusCode)
		worker.guestBody = "" // clear cache so we don't loop
		return p.quickSubmit(worker, guestURL, date, timeSlot)
	}

	log.Printf("[QUICKSUBMIT] worker=%d submit failed status=%d body=%q", worker.id, resp2.StatusCode, shortBody(body2))
	return &Result{Success: false, Message: fmt.Sprintf("Submit status: %d", resp2.StatusCode)}
}

func (p *PreWarmClient) quickConfirm(worker *PreWarmedWorker, timeSlot string) *Result {
	confURL := p.baseURL + "/reservations/conf"

	// Try fast path: use pre-fetched conf tokens
	if worker.confCsrf != "" && worker.confFields != "" {
		formData := url.Values{}
		formData.Set("_method", "POST")
		formData.Set("_csrfToken", worker.confCsrf)
		formData.Set("_Token[fields]", worker.confFields)
		formData.Set("_Token[unlocked]", worker.confUnlocked)

		req, _ := http.NewRequest("POST", confURL, strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Origin", p.baseURL)
		req.Header.Set("Referer", confURL)

		confPostStart := time.Now()
		resp2, err := worker.client.Do(req)
		if err != nil {
			log.Printf("[QUICKCONFIRM] worker=%d conf POST error: %v", worker.id, err)
			goto slowPath
		}
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		log.Printf("[QUICKCONFIRM] worker=%d conf POST (cached) status=%d latency=%v",
			worker.id, resp2.StatusCode, time.Since(confPostStart).Round(time.Millisecond))

		if resp2.StatusCode == 302 {
			location := resp2.Header.Get("Location")
			if strings.Contains(location, "/finish/") {
				log.Printf("[QUICKCONFIRM] ★ worker=%d BOOKING SUCCESS! slot=%s location=%s", worker.id, timeSlot, location)
				return &Result{Success: true, TimeSlot: timeSlot, Message: "Booked: " + location}
			}
		}

		// Cached tokens rejected, fall back to slow path
		worker.confCsrf = ""
		log.Printf("[QUICKCONFIRM] worker=%d cached conf rejected, retrying with fresh GET", worker.id)
	}

slowPath:
	log.Printf("[QUICKCONFIRM] worker=%d GET conf page for slot=%s", worker.id, timeSlot)
	confStart := time.Now()

	req, _ := http.NewRequest("GET", confURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", p.baseURL+"/reservations/user/guest")

	resp, err := worker.client.Do(req)
	if err != nil {
		log.Printf("[QUICKCONFIRM] worker=%d conf GET error: %v", worker.id, err)
		return &Result{Success: false, Message: fmt.Sprintf("Conf failed: %v", err)}
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("[QUICKCONFIRM] worker=%d conf GET status=%d bodyLen=%d latency=%v",
		worker.id, resp.StatusCode, len(body), time.Since(confStart).Round(time.Millisecond))

	if resp.StatusCode != 200 {
		log.Printf("[QUICKCONFIRM] Conf GET status=%d (expected 200), body=%q", resp.StatusCode, shortBody(body))
		return &Result{Success: false, Message: fmt.Sprintf("Conf page status: %d", resp.StatusCode)}
	}

	tokenCsrf, tokenFields, tokenUnlocked := extractFormTokens(string(body))
	if tokenCsrf == "" || tokenFields == "" {
		log.Printf("[QUICKCONFIRM] Missing tokens on conf page: csrf=%q fields=%q", tokenCsrf, tokenFields)
		return &Result{Success: false, Message: "No form tokens on conf page"}
	}

	formData := url.Values{}
	formData.Set("_method", "POST")
	formData.Set("_csrfToken", tokenCsrf)
	formData.Set("_Token[fields]", tokenFields)
	formData.Set("_Token[unlocked]", tokenUnlocked)

	req2, _ := http.NewRequest("POST", confURL, strings.NewReader(formData.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", userAgent)
	req2.Header.Set("Origin", p.baseURL)
	req2.Header.Set("Referer", confURL)

	confPostStart := time.Now()
	resp2, err := worker.client.Do(req2)
	if err != nil {
		log.Printf("[QUICKCONFIRM] worker=%d conf POST error: %v", worker.id, err)
		return &Result{Success: false, Message: fmt.Sprintf("Confirm failed: %v", err)}
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	log.Printf("[QUICKCONFIRM] worker=%d conf POST status=%d latency=%v",
		worker.id, resp2.StatusCode, time.Since(confPostStart).Round(time.Millisecond))

	if resp2.StatusCode == 302 {
		location := resp2.Header.Get("Location")
		log.Printf("[QUICKCONFIRM] worker=%d 302 → %s", worker.id, location)
		if strings.Contains(location, "/finish/") {
			log.Printf("[QUICKCONFIRM] ★ worker=%d BOOKING SUCCESS! slot=%s location=%s", worker.id, timeSlot, location)
			return &Result{Success: true, TimeSlot: timeSlot, Message: "Booked: " + location}
		}
		log.Printf("[QUICKCONFIRM] worker=%d 302 but NOT /finish/ → booking may have failed", worker.id)
	}
	log.Printf("[QUICKCONFIRM] worker=%d confirm failed status=%d body=%q", worker.id, resp2.StatusCode, shortBody(body2))
	return &Result{Success: false, Message: fmt.Sprintf("Confirm status: %d", resp2.StatusCode)}
}

// getCookieCSRF extracts the csrfToken cookie value from the worker's jar.
func (p *PreWarmClient) getCookieCSRF(worker *PreWarmedWorker) string {
	u, _ := url.Parse(p.baseURL)
	for _, c := range worker.jar.Cookies(u) {
		if c.Name == "csrfToken" {
			return c.Value
		}
	}
	return ""
}

// KeepAlive pings the calendar page every 30 seconds to prevent session expiry.
// Workers are pinged in small batches to avoid overwhelming the server.
// Checks stop channel before each batch to ensure prompt shutdown.
func (p *PreWarmClient) KeepAlive(stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Printf("[KEEPALIVE] Stopped")
			return
		case <-ticker.C:
			// Re-check stop before starting the (slow) batch work.
			select {
			case <-stop:
				log.Printf("[KEEPALIVE] Stopped (before batch)")
				return
			default:
			}
			const batchSize = 5
			for i := 0; i < len(p.clients); i += batchSize {
				// Check stop between batches to avoid running during burst.
				select {
				case <-stop:
					log.Printf("[KEEPALIVE] Stopped (mid-batch)")
					return
				default:
				}
				end := i + batchSize
				if end > len(p.clients) {
					end = len(p.clients)
				}
				var wg sync.WaitGroup
				for _, worker := range p.clients[i:end] {
					if worker.csrfToken == "" {
						continue
					}
					wg.Add(1)
					go func(w *PreWarmedWorker) {
						defer wg.Done()
						req, _ := http.NewRequest("GET", p.baseURL+"/reservations/calendar", nil)
						req.Header.Set("User-Agent", userAgent)
						resp, err := w.client.Do(req)
						if err == nil {
							drainBody(resp)
						}
					}(worker)
				}
				wg.Wait()
			}
			log.Printf("[KEEPALIVE] Sessions refreshed")
		}
	}
}

// ProbeSlots uses worker 0 to check a single slot and logs the raw response.
// Call this right before burst to verify session validity and see what the server returns.
func (p *PreWarmClient) ProbeSlots(date string) {
	slots := GetTimeSlots()
	w := p.clients[0]
	if w.csrfToken == "" {
		log.Printf("[PROBE] Worker 0 has no session, skipping")
		return
	}

	for _, slot := range slots[:3] { // probe first 3 slots only
		optionURL := fmt.Sprintf("%s/reservations/option?event_id=%s&event_plan_id=%s&date=%s&time_from=%s",
			p.baseURL, p.cfg.EventID, p.cfg.PlanID, url.QueryEscape(date), url.QueryEscape(slot))

		req, _ := http.NewRequest("GET", optionURL, nil)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Referer", p.baseURL+"/reservations/calendar")

		resp, err := w.client.Do(req)
		if err != nil {
			log.Printf("[PROBE] slot=%s error=%v", slot, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 302 {
			log.Printf("[PROBE] slot=%s status=302 location=%s", slot, resp.Header.Get("Location"))
		} else if resp.StatusCode == 200 {
			log.Printf("[PROBE] slot=%s status=200 ← SLOT AVAILABLE (option page loaded)", slot)
		} else {
			log.Printf("[PROBE] slot=%s status=%d bodyLen=%d body=%q", slot, resp.StatusCode, len(body), shortBody(body))
		}
	}
}
