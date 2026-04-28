package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const webhookTimeout = 5 * time.Second

// SendJSON sends a JSON payload to a configured webhook URL with a bounded
// timeout and basic SSRF protection for localhost/private IP targets.
func SendJSON(webhookURL string, payload any) error {
	parsed, err := validateWebhookURL(webhookURL)
	if err != nil {
		return err
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", parsed.String(), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "visa-monitor/1.0")

	client := &http.Client{
		Timeout: webhookTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           safeWebhookDialContext,
			TLSHandshakeTimeout:   webhookTimeout,
			ResponseHeaderTimeout: webhookTimeout,
			IdleConnTimeout:       webhookTimeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if _, err := validateWebhookURL(req.URL.String()); err != nil {
				return err
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many webhook redirects")
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if readErr != nil {
			return fmt.Errorf("webhook status %d; failed reading response body: %w", resp.StatusCode, readErr)
		}
		return fmt.Errorf("webhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("drain webhook response: %w", err)
	}
	return nil
}

func safeWebhookDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse webhook dial address: %w", err)
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook host: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve webhook host: no addresses")
	}

	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip.IP)
		if !ok || isBlockedAddress(addr) {
			return nil, fmt.Errorf("webhook host resolves to private or local address")
		}
	}

	var dialer net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial webhook host: %w", lastErr)
}

func validateWebhookURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("webhook_url is empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse webhook_url: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("webhook_url must use http or https")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("webhook_url must not include credentials")
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("webhook_url must include a host")
	}

	normalizedHost := strings.TrimSuffix(strings.ToLower(host), ".")
	if normalizedHost == "localhost" || strings.HasSuffix(normalizedHost, ".localhost") {
		return nil, fmt.Errorf("webhook_url must not target localhost")
	}

	if addr, err := netip.ParseAddr(normalizedHost); err == nil {
		if isBlockedAddress(addr) {
			return nil, fmt.Errorf("webhook_url must not target private or local addresses")
		}
		return parsed, nil
	}

	if ip := net.ParseIP(normalizedHost); ip != nil {
		addr, ok := netip.AddrFromSlice(ip)
		if ok && isBlockedAddress(addr.Unmap()) {
			return nil, fmt.Errorf("webhook_url must not target private or local addresses")
		}
	}

	return parsed, nil
}

func isBlockedAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}
