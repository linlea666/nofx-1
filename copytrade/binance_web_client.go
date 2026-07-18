package copytrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

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
		return raw, resp.StatusCode, fmt.Errorf("binance http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, resp.StatusCode, nil
}
