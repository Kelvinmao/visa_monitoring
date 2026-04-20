package booking

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"visa_monitor/internal/config"
)

type Result struct {
	Success   bool
	Date      string
	TimeSlot  string
	Message   string
	Timestamp time.Time
}

type Client struct {
	cfg       *config.Config
	client    *http.Client
	mu        sync.Mutex
	sessionOK bool
}

func NewClient(cfg *config.Config) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		cfg: cfg,
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     30 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SplitPhone splits a phone string into [area, prefix, line] digit groups.
func SplitPhone(phone string) [3]string {
	digits := regexp.MustCompile(`[^0-9]`).ReplaceAllString(phone, "")
	if len(digits) >= 10 {
		return [3]string{digits[:3], digits[3:6], digits[6:10]}
	} else if len(digits) >= 7 {
		return [3]string{digits[:3], digits[3:6], digits[6:]}
	} else if len(digits) >= 4 {
		return [3]string{digits[:3], digits[3:], ""}
	}
	return [3]string{digits, "", ""}
}

func (c *Client) InitSession(date string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	log.Printf("[INIT] Establishing session for %s", date)

	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/reservations/calendar", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	reCsrf := regexp.MustCompile(`name="_csrfToken"[^>]*value="([^"]+)"`)
	csrfMatch := reCsrf.FindStringSubmatch(string(body))
	if len(csrfMatch) < 2 {
		return fmt.Errorf("no CSRF token")
	}

	formData := url.Values{}
	formData.Set("event", c.cfg.EventID)
	formData.Set("plan", c.cfg.PlanID)
	formData.Set("date", date)
	formData.Set("disp_type", "day")
	formData.Set("search", "exec")
	formData.Set("_csrfToken", csrfMatch[1])

	req2, _ := http.NewRequest("POST", c.cfg.BaseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req2.Header.Set("X-Requested-With", "XMLHttpRequest")
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp2, err := c.client.Do(req2)
	if err != nil {
		return err
	}
	resp2.Body.Close()

	c.sessionOK = true
	log.Printf("[INIT] Session ready")
	return nil
}

func (c *Client) extractCSRFToken() (string, error) {
	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/reservations/calendar", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	re := regexp.MustCompile(`<input[^>]+name="_csrfToken"[^>]+value="([^"]+)"`)
	matches := re.FindStringSubmatch(string(body))

	if len(matches) > 1 {
		return matches[1], nil
	}

	return "", fmt.Errorf("no CSRF token found")
}

func (c *Client) CheckAvailableSlots(date string) ([]map[string]string, error) {
	csrfToken, err := c.extractCSRFToken()
	if err != nil {
		return nil, err
	}

	formData := url.Values{}
	formData.Set("event", c.cfg.EventID)
	formData.Set("plan", c.cfg.PlanID)
	formData.Set("date", date)
	formData.Set("disp_type", "day")
	formData.Set("search", "exec")
	formData.Set("_csrfToken", csrfToken)

	req, _ := http.NewRequest("POST", c.cfg.BaseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	reHtml := regexp.MustCompile(`"html":"([^"]*)"`)
	htmlMatches := reHtml.FindStringSubmatch(string(body))
	if len(htmlMatches) < 2 {
		return nil, nil
	}

	html := strings.ReplaceAll(htmlMatches[1], "\\n", "\n")
	html = strings.ReplaceAll(html, "\\\"", "\"")

	reSlot := regexp.MustCompile(`<a[^>]+data-event-id="([^"]+)"[^>]+data-plan-id="([^"]+)"[^>]+data-date="([^"]+)"[^>]+data-time_from="([^"]+)"[^>]*>`)
	matches := reSlot.FindAllStringSubmatch(html, -1)

	slots := make([]map[string]string, 0)
	for _, match := range matches {
		timeFrom := match[4]
		if strings.Contains(timeFrom, ":") {
			parts := strings.Split(timeFrom, ":")
			if len(parts) >= 2 {
				timeFrom = parts[0] + ":" + parts[1]
			}
		}

		slots = append(slots, map[string]string{
			"event_id": match[1],
			"plan_id":  match[2],
			"date":     match[3],
			"time":     timeFrom,
		})
	}

	return slots, nil
}

func (c *Client) Book(date, timeSlot string) *Result {
	result := &Result{
		Date:      date,
		TimeSlot:  timeSlot,
		Timestamp: time.Now(),
	}

	if !c.sessionOK {
		log.Printf("[DEBUG] Step 1: 访问日历页面建立 Session")
		req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/reservations/calendar", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		resp, err := c.client.Do(req)
		if err != nil {
			result.Message = fmt.Sprintf("Calendar page failed: %v", err)
			return result
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		reCsrf := regexp.MustCompile(`name="_csrfToken"[^>]*value="([^"]+)"`)
		csrfMatch := reCsrf.FindStringSubmatch(string(body))
		csrfToken := ""
		if len(csrfMatch) > 1 {
			csrfToken = csrfMatch[1]
		}

		log.Printf("[DEBUG] Step 2: 提交日历查询 event=%s, plan=%s", c.cfg.EventID, c.cfg.PlanID)
		formData := url.Values{}
		formData.Set("event", c.cfg.EventID)
		formData.Set("plan", c.cfg.PlanID)
		formData.Set("date", date)
		formData.Set("disp_type", "day")
		formData.Set("search", "exec")
		formData.Set("_csrfToken", csrfToken)

		req2, _ := http.NewRequest("POST", c.cfg.BaseURL+"/ajax/reservations/calendar", strings.NewReader(formData.Encode()))
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req2.Header.Set("X-Requested-With", "XMLHttpRequest")
		req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		req2.Header.Set("Referer", c.cfg.BaseURL+"/reservations/calendar")
		resp2, err := c.client.Do(req2)
		if err != nil {
			result.Message = fmt.Sprintf("Calendar query failed: %v", err)
			return result
		}
		resp2.Body.Close()
	}

	log.Printf("[DEBUG] Step 3: 访问 option 页面")
	optionURL := fmt.Sprintf("/reservations/option?event_id=%s&event_plan_id=%s&date=%s&time_from=%s",
		c.cfg.EventID, c.cfg.PlanID, url.QueryEscape(date), url.QueryEscape(timeSlot))

	req, err := http.NewRequest("GET", c.cfg.BaseURL+optionURL, nil)
	if err != nil {
		result.Message = fmt.Sprintf("Create request failed: %v", err)
		return result
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", c.cfg.BaseURL+"/reservations/calendar")

	resp, err := c.client.Do(req)
	if err != nil {
		result.Message = fmt.Sprintf("Option request failed: %v", err)
		return result
	}

	log.Printf("[DEBUG] Option response status: %d", resp.StatusCode)

	if resp.StatusCode != 200 && resp.StatusCode < 300 || resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("[DEBUG] Option error response: %s", string(bodyBytes)[:min(500, len(bodyBytes))])
		result.Message = fmt.Sprintf("Option status: %d", resp.StatusCode)
		return result
	}

	guestURL := "/reservations/user/guest"
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if location := resp.Header.Get("Location"); location != "" {
			log.Printf("[DEBUG] Redirect to: %s", location)
			if strings.HasPrefix(location, c.cfg.BaseURL) {
				guestURL = location[len(c.cfg.BaseURL):]
			} else {
				guestURL = location
			}
		}
	} else {
		resp.Body.Close()
	}

	if !strings.HasPrefix(guestURL, "http") {
		guestURL = c.cfg.BaseURL + guestURL
	}

	log.Printf("[DEBUG] Step 4: 访问 guest 页面: %s", guestURL)
	req2, err := http.NewRequest("GET", guestURL, nil)
	if err != nil {
		result.Message = fmt.Sprintf("Create request failed: %v", err)
		return result
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req2.Header.Set("Referer", c.cfg.BaseURL+"/reservations/option")

	resp, err = c.client.Do(req2)
	if err != nil {
		result.Message = fmt.Sprintf("Guest page failed: %v", err)
		return result
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	reCsrf2 := regexp.MustCompile(`<input[^>]+name="_csrfToken"[^>]+value="([^"]+)"`)
	csrfMatches := reCsrf2.FindStringSubmatch(string(body))

	reFields := regexp.MustCompile(`<input[^>]+name="_Token\[fields\]"[^>]+value="([^"]+)"`)
	fieldsMatches := reFields.FindStringSubmatch(string(body))

	reUnlocked := regexp.MustCompile(`<input[^>]+name="_Token\[unlocked\]"[^>]+value="([^"]*)"`)
	unlockedMatches := reUnlocked.FindStringSubmatch(string(body))

	tokenCsrf := ""
	tokenFields := ""
	tokenUnlocked := ""

	if len(csrfMatches) > 1 {
		tokenCsrf = csrfMatches[1]
	}
	if len(fieldsMatches) > 1 {
		tokenFields = fieldsMatches[1]
	}
	if len(unlockedMatches) > 1 {
		tokenUnlocked = unlockedMatches[1]
	}

	log.Printf("[DEBUG] Guest page tokens: CSRF=%s..., Fields=%s...",
		tokenCsrf[:min(20, len(tokenCsrf))], tokenFields[:min(20, len(tokenFields))])

	log.Printf("[DEBUG] Step 5: 提交预约表单")
	parts := SplitPhone(c.cfg.Phone)

	formData2 := url.Values{}
	formData2.Set("_method", "POST")
	formData2.Set("_csrfToken", tokenCsrf)
	formData2.Set("users[addition_values][4][0]", c.cfg.FamilyName)
	formData2.Set("users[addition_values][4][1]", c.cfg.FirstName)
	formData2.Set("users[addition_values][6][0]", parts[0])
	formData2.Set("users[addition_values][6][1]", parts[1])
	formData2.Set("users[addition_values][6][2]", parts[2])
	formData2.Set("users[addition_values][16]", "")
	formData2.Set("users[addition_values][17]", "")
	formData2.Set("users[mail]", c.cfg.Email)
	formData2.Set("users[mail_confirm]", c.cfg.Email)
	formData2.Set("_Token[fields]", tokenFields)
	formData2.Set("_Token[unlocked]", tokenUnlocked)

	postReq, _ := http.NewRequest("POST", guestURL, strings.NewReader(formData2.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	postReq.Header.Set("Referer", guestURL)
	postReq.Header.Set("Origin", c.cfg.BaseURL)

	postResp, err := c.client.Do(postReq)
	if err != nil {
		result.Message = fmt.Sprintf("Submit failed: %v", err)
		return result
	}
	postResp.Body.Close()

	log.Printf("[DEBUG] Form submit status: %d", postResp.StatusCode)

	if postResp.StatusCode >= 200 && postResp.StatusCode < 400 {
		result.Message = "Submitted successfully"
		if c.confirm() {
			result.Success = true
			result.Message = "Booking confirmed!"
		}
	} else {
		result.Message = fmt.Sprintf("Submit status: %d", postResp.StatusCode)
	}

	return result
}

func (c *Client) confirm() bool {
	log.Printf("[DEBUG] Step 6: 访问确认页面")
	req, _ := http.NewRequest("GET", c.cfg.BaseURL+"/reservations/conf", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	reCsrf := regexp.MustCompile(`<input[^>]+name="_csrfToken"[^>]+value="([^"]+)"`)
	reFields := regexp.MustCompile(`<input[^>]+name="_Token\[fields\]"[^>]+value="([^"]+)"`)
	reUnlocked := regexp.MustCompile(`<input[^>]+name="_Token\[unlocked\]"[^>]+value="([^"]*)"`)

	csrfMatch := reCsrf.FindStringSubmatch(string(body))
	fieldsMatch := reFields.FindStringSubmatch(string(body))
	unlockedMatch := reUnlocked.FindStringSubmatch(string(body))

	payload := url.Values{}
	payload.Set("_method", "POST")
	if len(csrfMatch) > 1 {
		payload.Set("_csrfToken", csrfMatch[1])
	}
	if len(fieldsMatch) > 1 {
		payload.Set("_Token[fields]", fieldsMatch[1])
	}
	if len(unlockedMatch) > 1 {
		payload.Set("_Token[unlocked]", unlockedMatch[1])
	}

	log.Printf("[DEBUG] Step 7: 提交确认")
	req2, _ := http.NewRequest("POST", c.cfg.BaseURL+"/reservations/conf", strings.NewReader(payload.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req2.Header.Set("Referer", c.cfg.BaseURL+"/reservations/conf")
	req2.Header.Set("Origin", c.cfg.BaseURL)

	resp2, err := c.client.Do(req2)
	if err != nil {
		return false
	}
	resp2.Body.Close()

	log.Printf("[DEBUG] Confirm status: %d", resp2.StatusCode)

	if resp2.StatusCode >= 300 && resp2.StatusCode < 400 {
		location := resp2.Header.Get("Location")
		log.Printf("[DEBUG] Redirect to: %s", location)
		if strings.Contains(location, "/finish/") {
			log.Printf("[DEBUG] ✅ 预约成功！")
			return true
		}
	}

	return false
}



func GetTimeSlots() []string {
	return []string{
		"09:00", "09:15", "09:30", "09:45",
		"10:00", "10:15", "10:30", "10:45",
		"11:00", "11:15", "11:30", "11:45",
	}
}
