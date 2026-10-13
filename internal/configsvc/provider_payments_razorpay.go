package configsvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// razorpayBaseURL is a variable so tests can point the check at a local server.
var razorpayBaseURL = "https://api.razorpay.com"

// RazorpayBaseURL is where gateway calls go.
func RazorpayBaseURL() string { return razorpayBaseURL }

// SetRazorpayBaseURLForTest redirects gateway calls and returns a restore func.
func SetRazorpayBaseURLForTest(u string) (restore func()) {
	prev := razorpayBaseURL
	razorpayBaseURL = strings.TrimRight(u, "/")
	return func() { razorpayBaseURL = prev }
}

// CheckRazorpay verifies key_id/key_secret with a read-only call. It never
// creates an order or moves money.
func CheckRazorpay(ctx context.Context, baseURL, keyID, keySecret string) error {
	if keyID == "" || keySecret == "" {
		return errors.New("key id and key secret are both required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/orders?count=1", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(keyID, keySecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the gateway: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New("the gateway rejected these credentials")
	default:
		return fmt.Errorf("the gateway answered %d", resp.StatusCode)
	}
}
