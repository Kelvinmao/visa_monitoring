package booking

import (
	"net"
	"net/http"
	"regexp"
	"time"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Pre-compiled regex patterns for token extraction.
var (
	reCsrfValue     = regexp.MustCompile(`name="_csrfToken"[^>]*value="([^"]+)"`)
	reTokenFields   = regexp.MustCompile(`name="_Token\[fields\]"[^>]*value="([^"]+)"`)
	reTokenUnlocked = regexp.MustCompile(`name="_Token\[unlocked\]"[^>]*value="([^"]*)"`)
	reFormAction    = regexp.MustCompile(`<form[^>]*action="([^"]*)"`)
)

// buildTransport returns a shared HTTP transport config.
// No custom TLS ServerName or IP-pinning — the domain is used directly.
func buildTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        300,
		MaxIdleConnsPerHost: 300,
		MaxConnsPerHost:     300,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
}

// extractFormTokens pulls the three CakePHP security tokens from an HTML page.
func extractFormTokens(body string) (csrf, fields, unlocked string) {
	if m := reCsrfValue.FindStringSubmatch(body); len(m) > 1 {
		csrf = m[1]
	}
	if m := reTokenFields.FindStringSubmatch(body); len(m) > 1 {
		fields = m[1]
	}
	if m := reTokenUnlocked.FindStringSubmatch(body); len(m) > 1 {
		unlocked = m[1]
	}
	return
}

// ExtractFormTokensPublic is a public wrapper for diagnose tooling.
func ExtractFormTokensPublic(body string) (csrf, fields, unlocked string) {
	return extractFormTokens(body)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
