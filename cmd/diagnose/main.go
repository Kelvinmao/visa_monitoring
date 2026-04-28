package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"visa_monitor/internal/booking"
	"visa_monitor/internal/config"
)

// diagnose: step-by-step booking with full response logging.
func main() {
	configPath := flag.String("config", "config_test_booking.json", "Config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	jar, _ := booking.NewThreadSafeJar()
	client := &http.Client{
		Jar:     jar,
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	baseURL := cfg.BaseURL
	slot := "09:00"
	date := cfg.TargetDate

	// Step 1: GET calendar page for CSRF
	log.Println("=== Step 1: GET calendar ===")
	req := mustNewRequest("GET", baseURL+"/reservations/calendar", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Step 1 failed: %v", err)
	}
	body := mustReadBody("Step 1", resp)
	log.Printf("Status: %d, BodyLen: %d", resp.StatusCode, len(body))

	csrf, _, _ := booking.ExtractFormTokensPublic(string(body))
	log.Printf("CSRF: %s", csrf)

	// Step 2: POST calendar to set session
	log.Println("=== Step 2: POST calendar (set session) ===")
	formData := url.Values{}
	formData.Set("event", cfg.EventID)
	formData.Set("plan", cfg.PlanID)
	formData.Set("date", date)
	formData.Set("disp_type", "day")
	formData.Set("search", "exec")
	formData.Set("_csrfToken", csrf)

	req2 := mustNewRequest("POST", baseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req2.Header.Set("X-Requested-With", "XMLHttpRequest")
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	resp2, err := client.Do(req2)
	if err != nil {
		log.Fatalf("Step 2 failed: %v", err)
	}
	body2 := mustReadBody("Step 2", resp2)
	log.Printf("Status: %d, BodyLen: %d", resp2.StatusCode, len(body2))

	// Step 3: GET option page
	log.Println("=== Step 3: GET option ===")
	optionURL := fmt.Sprintf("%s/reservations/option?event_id=%s&event_plan_id=%s&date=%s&time_from=%s",
		baseURL, cfg.EventID, cfg.PlanID, url.QueryEscape(date), url.QueryEscape(slot))
	req3 := mustNewRequest("GET", optionURL, nil)
	req3.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req3.Header.Set("Referer", baseURL+"/reservations/calendar")
	resp3, err := client.Do(req3)
	if err != nil {
		log.Fatalf("Step 3 failed: %v", err)
	}
	body3 := mustReadBody("Step 3", resp3)
	log.Printf("Status: %d, BodyLen: %d", resp3.StatusCode, len(body3))
	if resp3.StatusCode == 302 {
		loc := resp3.Header.Get("Location")
		log.Printf("Redirect: %s", loc)

		// Follow redirect
		if !strings.HasPrefix(loc, "http") {
			loc = baseURL + loc
		}
		req3b := mustNewRequest("GET", loc, nil)
		req3b.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		resp3b, err := client.Do(req3b)
		if err != nil {
			log.Fatalf("Step 3b (follow redirect) failed: %v", err)
		}
		body3 = mustReadBody("Step 3b", resp3b)
		log.Printf("After redirect - Status: %d, BodyLen: %d", resp3b.StatusCode, len(body3))
	}

	// Check if option page has a form to submit
	csrfOpt, fieldsOpt, unlockedOpt := booking.ExtractFormTokensPublic(string(body3))
	log.Printf("Option page tokens: csrf=%q fields=%q unlocked=%q", csrfOpt, fieldsOpt, unlockedOpt)

	// If option page has a form, submit it
	if csrfOpt != "" {
		log.Println("=== Step 3c: POST option form ===")
		optForm := url.Values{}
		optForm.Set("_method", "POST")
		optForm.Set("_csrfToken", csrfOpt)
		if fieldsOpt != "" {
			optForm.Set("_Token[fields]", fieldsOpt)
		}
		if unlockedOpt != "" {
			optForm.Set("_Token[unlocked]", unlockedOpt)
		}
		req3c := mustNewRequest("POST", baseURL+"/reservations/option", strings.NewReader(optForm.Encode()))
		req3c.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req3c.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req3c.Header.Set("Origin", baseURL)
		resp3c, err := client.Do(req3c)
		if err != nil {
			log.Fatalf("Step 3c failed: %v", err)
		}
		body3c := mustReadBody("Step 3c", resp3c)
		log.Printf("Status: %d, BodyLen: %d", resp3c.StatusCode, len(body3c))
		if resp3c.StatusCode == 302 {
			log.Printf("Redirect: %s", resp3c.Header.Get("Location"))
		}
	}

	// Step 4: GET guest page
	log.Println("=== Step 4: GET guest page ===")
	guestURL := baseURL + "/reservations/user/guest"
	req4 := mustNewRequest("GET", guestURL, nil)
	req4.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req4.Header.Set("Referer", baseURL+"/reservations/option")
	resp4, err := client.Do(req4)
	if err != nil {
		log.Fatalf("Step 4 failed: %v", err)
	}
	body4 := mustReadBody("Step 4", resp4)
	log.Printf("Status: %d, BodyLen: %d", resp4.StatusCode, len(body4))

	if resp4.StatusCode == 302 {
		log.Printf("Redirect to: %s", resp4.Header.Get("Location"))
		log.Println("Guest page redirected — session may have expired or option step was skipped")
		return
	}

	csrfGuest, fieldsGuest, unlockedGuest := booking.ExtractFormTokensPublic(string(body4))
	log.Printf("Guest tokens: csrf=%q fields=%.30s... unlocked=%q", csrfGuest, fieldsGuest, unlockedGuest)

	if csrfGuest == "" {
		log.Println("No CSRF token on guest page — dumping first 2000 chars:")
		if len(body4) > 2000 {
			fmt.Println(string(body4[:2000]))
		} else {
			fmt.Println(string(body4))
		}
		return
	}

	// Step 5: POST guest form
	log.Println("=== Step 5: POST guest form ===")
	parts := booking.SplitPhone(cfg.Phone)
	guestForm := url.Values{}
	guestForm.Set("_method", "POST")
	guestForm.Set("_csrfToken", csrfGuest)
	guestForm.Set("users[addition_values][4][0]", cfg.FamilyName)
	guestForm.Set("users[addition_values][4][1]", cfg.FirstName)
	guestForm.Set("users[addition_values][6][0]", parts[0])
	guestForm.Set("users[addition_values][6][1]", parts[1])
	guestForm.Set("users[addition_values][6][2]", parts[2])
	guestForm.Set("users[mail]", cfg.Email)
	guestForm.Set("users[mail_confirm]", cfg.Email)
	guestForm.Set("users[addition_values][16]", "")
	guestForm.Set("users[addition_values][17]", "")
	guestForm.Set("_Token[fields]", fieldsGuest)
	guestForm.Set("_Token[unlocked]", unlockedGuest)

	log.Printf("Submitting form data:")
	for k, v := range guestForm {
		log.Printf("  %s = %s", k, v[0])
	}

	req5 := mustNewRequest("POST", guestURL, strings.NewReader(guestForm.Encode()))
	req5.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req5.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req5.Header.Set("Origin", baseURL)
	req5.Header.Set("Referer", guestURL)

	resp5, err := client.Do(req5)
	if err != nil {
		log.Fatalf("Step 5 failed: %v", err)
	}
	body5 := mustReadBody("Step 5", resp5)
	log.Printf("Status: %d, BodyLen: %d", resp5.StatusCode, len(body5))
	if resp5.StatusCode == 302 {
		log.Printf("Redirect: %s — SUCCESS, guest form accepted!", resp5.Header.Get("Location"))
	} else {
		log.Println("Guest POST returned 200 — form rejected. Writing full body to /tmp/guest_response.html")
		_ = writeFile("/tmp/guest_response.html", body5)
		fmt.Println(string(body5[:min5(3000, len(body5))]))
	}
}

func mustNewRequest(method, requestURL string, body io.Reader) *http.Request {
	req, err := http.NewRequest(method, requestURL, body)
	if err != nil {
		log.Fatalf("Create %s request failed for %s: %v", method, requestURL, err)
	}
	return req
}

func mustReadBody(step string, resp *http.Response) []byte {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatalf("%s response read failed: %v", step, err)
	}
	return body
}

func writeFile(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func min5(a, b int) int {
	if a < b {
		return a
	}
	return b
}
