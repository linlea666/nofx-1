package copytrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BinanceHTTPError preserves response metadata required by the Smart Money
// source scheduler. Callers must not parse error strings to implement 429
// backoff or source-health classification.
type BinanceHTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *BinanceHTTPError) Error() string {
	if e == nil {
		return "binance http error"
	}
	return fmt.Sprintf("binance http %d: %s", e.StatusCode, e.Body)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// binanceWebRequest centralizes the authenticated Binance web request
// contract shared by copy-management and Smart Money providers. It never logs
// credentials or response headers.
func binanceWebRequest(client *http.Client, p20t, csrf, method, url string, body interface{}) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("clienttype", "web")
	req.Header.Set("csrftoken", csrf)
	req.Header.Set("user-agent", "Mozilla/5.0 (compatible; NOFX/1.0)")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: "p20t", Value: p20t})

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("binance request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("binance read body failed: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return raw, resp.StatusCode, ErrBinanceCredentialsExpired
	}
	if resp.StatusCode != http.StatusOK {
		return raw, resp.StatusCode, &BinanceHTTPError{
			StatusCode: resp.StatusCode,
			Body:       truncate(string(raw), 200),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	return raw, resp.StatusCode, nil
}
