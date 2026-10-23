// Command e2e runs inside a container on the same network as the gateway and the
// mock internal service. It fails the process when a check does not hold.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	gatewayURL := getenv("GATEWAY_URL", "http://gateway:8080")
	mockURL := getenv("MOCK_URL", "http://mock:8080")
	client := &http.Client{Timeout: 5 * time.Second}

	steps := []struct {
		name string
		fn   func() error
	}{
		{"gateway is up", func() error { return waitReady(client, gatewayURL+"/healthz") }},
		{"mock is up", func() error { return waitReady(client, mockURL+"/healthz") }},
		{"session is issued and reused", func() error { return checkSession(client, gatewayURL) }},
		{"rate limit is per session", func() error { return checkRateLimit(client, gatewayURL, mockURL) }},
		{"circuit breaker stops a failing upstream", func() error { return checkBreaker(client, gatewayURL, mockURL) }},
	}
	for _, s := range steps {
		fmt.Println("==", s.name)
		if err := s.fn(); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", s.name, err)
			os.Exit(1)
		}
	}
	fmt.Println("ok")
}

func checkSession(client *http.Client, gatewayURL string) error {
	res, body, err := do(client, http.MethodGet, gatewayURL+"/api/ping", "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("first status %d, body %s", res.StatusCode, body)
	}
	token := res.Header.Get("X-Session-Token")
	if token == "" {
		return fmt.Errorf("first response has no session token")
	}
	if res.Header.Get("X-Request-ID") == "" {
		return fmt.Errorf("response has no X-Request-ID")
	}
	var echoed struct {
		Path      string `json:"path"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &echoed); err != nil {
		return err
	}
	if echoed.Path != "/ping" {
		return fmt.Errorf("upstream path %q, want /ping", echoed.Path)
	}
	if echoed.SessionID == "" {
		return fmt.Errorf("upstream did not receive a session id")
	}

	res, body, err = do(client, http.MethodGet, gatewayURL+"/api/ping", token)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("second status %d, body %s", res.StatusCode, body)
	}
	if res.Header.Get("X-Session-Token") != "" {
		return fmt.Errorf("live session was replaced with a new token")
	}
	var again struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &again); err != nil {
		return err
	}
	if again.SessionID != echoed.SessionID {
		return fmt.Errorf("session changed from %s to %s", echoed.SessionID, again.SessionID)
	}
	return nil
}

func checkRateLimit(client *http.Client, gatewayURL, mockURL string) error {
	before, err := hits(client, mockURL)
	if err != nil {
		return err
	}
	res, _, err := do(client, http.MethodGet, gatewayURL+"/limited/x", "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("first limited status %d", res.StatusCode)
	}
	token := res.Header.Get("X-Session-Token")
	if token == "" {
		return fmt.Errorf("limited route did not issue a session")
	}
	res, _, err = do(client, http.MethodGet, gatewayURL+"/limited/x", token)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("second limited status %d", res.StatusCode)
	}
	res, _, err = do(client, http.MethodGet, gatewayURL+"/limited/x", token)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("third limited status %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		return fmt.Errorf("429 has no Retry-After")
	}
	got, err := hits(client, mockURL)
	if err != nil {
		return err
	}
	if got != before+2 {
		return fmt.Errorf("mock hits = %d, want %d (the 429 must not be forwarded)", got, before+2)
	}

	time.Sleep(1100 * time.Millisecond)
	res, _, err = do(client, http.MethodGet, gatewayURL+"/limited/x", token)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("after refill status %d, want 200", res.StatusCode)
	}
	return nil
}

func checkBreaker(client *http.Client, gatewayURL, mockURL string) error {
	if err := setMode(client, mockURL, "fail", true); err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		res, _, err := do(client, http.MethodGet, gatewayURL+"/api/down", "")
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusInternalServerError {
			return fmt.Errorf("failure %d status %d, want 500", i, res.StatusCode)
		}
	}
	got, err := hits(client, mockURL)
	if err != nil {
		return err
	}
	if got != 3 {
		return fmt.Errorf("hits after failures = %d, want 3", got)
	}

	res, _, err := do(client, http.MethodGet, gatewayURL+"/api/down", "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		return fmt.Errorf("open breaker status %d, want 503", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		return fmt.Errorf("503 has no Retry-After")
	}
	got, err = hits(client, mockURL)
	if err != nil {
		return err
	}
	if got != 3 {
		return fmt.Errorf("open breaker forwarded a request, hits = %d", got)
	}

	if err := setMode(client, mockURL, "ok", false); err != nil {
		return err
	}
	time.Sleep(2500 * time.Millisecond)

	res, _, err = do(client, http.MethodGet, gatewayURL+"/api/down", "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("probe status %d, want 200", res.StatusCode)
	}
	res, _, err = do(client, http.MethodGet, gatewayURL+"/api/down", "")
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("after close status %d, want 200", res.StatusCode)
	}
	got, err = hits(client, mockURL)
	if err != nil {
		return err
	}
	if got != 5 {
		return fmt.Errorf("hits after recovery = %d, want 5", got)
	}
	return nil
}

func setMode(client *http.Client, mockURL, mode string, reset bool) error {
	body, _ := json.Marshal(map[string]any{"mode": mode, "reset_hits": reset})
	req, err := http.NewRequest(http.MethodPost, mockURL+"/control", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("control status %d, body %s", res.StatusCode, b)
	}
	return nil
}

func hits(client *http.Client, mockURL string) (int, error) {
	res, err := client.Get(mockURL + "/hits")
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var body struct {
		Hits int `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.Hits, nil
}

func do(client *http.Client, method, url, token string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return res, body, err
}

func waitReady(client *http.Client, url string) error {
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		res, err := client.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", res.StatusCode)
		} else {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s: %v", url, last)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
