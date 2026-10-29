// Package notify delivers minimal, signed events without making approval decisions.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"openfactory/internal/model"
)

type Ledger interface {
	PendingNotifications(context.Context, string, int) ([]model.Event, error)
	AckNotification(context.Context, string, string) error
	RecordNotificationFailure(context.Context, string, string, string) error
}

type Dispatcher struct {
	URL        string
	SecretFile string
	Client     *http.Client
	Ledger     Ledger
}

func New(c model.Config, ledger Ledger) *Dispatcher {
	return &Dispatcher{URL: c.WebhookURL, SecretFile: c.WebhookSecretFile, Ledger: ledger, Client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	if d.URL == "" {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) Tick(ctx context.Context) error {
	if d.URL == "" {
		return nil
	}
	// Changing a destination gets a distinct delivery ledger; no old event is misattributed.
	sum := sha256.Sum256([]byte(d.URL))
	channel := "webhook:" + hex.EncodeToString(sum[:])
	events, err := d.Ledger.PendingNotifications(ctx, channel, 20)
	if err != nil {
		return err
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := d.Deliver(ctx, event); err != nil {
			if err = d.Ledger.RecordNotificationFailure(ctx, channel, event.ID, err.Error()); err != nil {
				return err
			}
		} else if err = d.Ledger.AckNotification(ctx, channel, event.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) Deliver(ctx context.Context, event model.Event) error {
	secret, err := os.ReadFile(d.SecretFile)
	if err != nil {
		return errors.New("webhook signing secret unavailable")
	}
	secret = bytes.TrimSpace(secret)
	if len(secret) < 32 || len(secret) > 4096 {
		return errors.New("webhook signing secret must contain 32..4096 bytes")
	}
	// Detail intentionally stays local: it can include repository content or upstream errors.
	payload := struct {
		Version   int       `json:"version"`
		ID        string    `json:"id"`
		FeatureID string    `json:"feature_id"`
		Kind      string    `json:"kind"`
		CreatedAt time.Time `json:"created_at"`
	}{1, event.ID, event.FeatureID, event.Kind, event.CreatedAt}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid webhook destination")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "openfactory/1")
	req.Header.Set("X-OpenFactory-Event-ID", event.ID)
	req.Header.Set("X-OpenFactory-Timestamp", timestamp)
	req.Header.Set("X-OpenFactory-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	client := d.Client
	if client == nil {
		return errors.New("webhook client unavailable")
	}
	// Redirect refusal is enforced even when a caller supplies a custom transport.
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return errors.New("webhook delivery uncertain; retry is at-least-once")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP status %d", resp.StatusCode)
	}
	return nil
}

// Verify checks the timestamp and signature over the exact raw body. Receivers
// must additionally deduplicate the event ID; a valid signature is not approval.
func Verify(secret, body []byte, timestamp, signature string, now time.Time) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if seconds < now.Unix()-300 || seconds > now.Unix()+30 {
		return false
	}
	supplied, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	return hmac.Equal(supplied, mac.Sum(nil))
}
