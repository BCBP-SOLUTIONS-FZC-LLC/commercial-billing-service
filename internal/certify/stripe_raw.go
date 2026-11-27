package certify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// StripeAPI is the suite's own Stripe access for what is not part of the
// adapter contract: creating the run's price book, reading raw events to
// deliver them to B-W1, and Test Clocks. Test-mode keys only (or
// stripe-mock); it never carries card data.
type StripeAPI struct {
	Key     string
	BaseURL string // https://api.stripe.com or the stripe-mock URL
	HTTP    *http.Client
}

func (s StripeAPI) do(ctx context.Context, method, path string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil && method != http.MethodGet {
		body = strings.NewReader(form.Encode())
	}
	u := strings.TrimRight(s.BaseURL, "/") + path
	if form != nil && method == http.MethodGet {
		u += "?" + form.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.Key, "")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return b, nil
}

func (s StripeAPI) id(ctx context.Context, method, path string, form url.Values) (string, error) {
	b, err := s.do(ctx, method, path, form)
	if err != nil {
		return "", err
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.ID == "" {
		return "", fmt.Errorf("stripe %s: no id in answer", path)
	}
	return v.ID, nil
}

// CreatePrice creates a product and a price; interval "" is one-off.
func (s StripeAPI) CreatePrice(ctx context.Context, name string, amountMinor int64, currency, interval string) (string, error) {
	prod, err := s.id(ctx, http.MethodPost, "/v1/products", url.Values{"name": {name}})
	if err != nil {
		return "", err
	}
	f := url.Values{"product": {prod}, "unit_amount": {strconv.FormatInt(amountMinor, 10)}, "currency": {strings.ToLower(currency)}}
	if interval != "" {
		f.Set("recurring[interval]", interval)
	}
	return s.id(ctx, http.MethodPost, "/v1/prices", f)
}

// RawEvent reads an event exactly as Stripe would deliver it.
func (s StripeAPI) RawEvent(ctx context.Context, id string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, "/v1/events/"+url.PathEscape(id), nil)
}

// Sign builds a Stripe-Signature header for body at t with secret
// (Stripe's documented v1 scheme: HMAC-SHA256 of "<t>.<body>").
func Sign(secret string, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(t.Unix(), 10)
	mac.Write([]byte(ts + "." + string(body)))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// TestClock is a Stripe Test Clock (test mode).
type TestClock struct {
	API StripeAPI
	ID  string
	now time.Time
}

// NewTestClock creates a clock frozen at start.
func NewTestClock(ctx context.Context, api StripeAPI, name string, start time.Time) (*TestClock, error) {
	id, err := api.id(ctx, http.MethodPost, "/v1/test_helpers/test_clocks",
		url.Values{"name": {name}, "frozen_time": {strconv.FormatInt(start.Unix(), 10)}})
	if err != nil {
		return nil, err
	}
	return &TestClock{API: api, ID: id, now: start.Truncate(time.Second)}, nil
}

// Now implements Clock.
func (c *TestClock) Now() time.Time { return c.now }

// Advance implements Clock: advances and waits until the clock is ready.
func (c *TestClock) Advance(ctx context.Context, to time.Time) error {
	if !to.After(c.now) {
		return nil
	}
	if _, err := c.API.do(ctx, http.MethodPost, "/v1/test_helpers/test_clocks/"+c.ID+"/advance",
		url.Values{"frozen_time": {strconv.FormatInt(to.Unix(), 10)}}); err != nil {
		return err
	}
	for range 120 {
		b, err := c.API.do(ctx, http.MethodGet, "/v1/test_helpers/test_clocks/"+c.ID, nil)
		if err != nil {
			return err
		}
		var v struct{ Status string }
		_ = json.Unmarshal(b, &v)
		switch v.Status {
		case "ready":
			c.now = to.Truncate(time.Second)
			return nil
		case "internal_failure":
			return fmt.Errorf("test clock %s failed to advance", c.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("test clock %s still advancing after 10 minutes", c.ID)
}

// Delete removes the clock and every object on it.
func (c *TestClock) Delete(ctx context.Context) error {
	_, err := c.API.do(ctx, http.MethodDelete, "/v1/test_helpers/test_clocks/"+c.ID, nil)
	return err
}

// AccountRef is the provider account the run used (record field).
func (s StripeAPI) AccountRef(ctx context.Context) (string, error) {
	return s.id(ctx, http.MethodGet, "/v1/account", nil)
}
