// lark.go — Lark/Feishu webhook client.
//
// Implements the official Lark custom-bot API:
//   POST <webhook_url> with body {timestamp, sign, msg_type, content}
//
// HMAC-SHA256 signing (per Lark's spec for "签名校验" mode):
//   string_to_sign = <timestamp>\n<secret>
//   sig = base64(HMAC-SHA256(string_to_sign, ""))     // empty data, key=string_to_sign
//
// Yes the sign formula is unusual — the secret goes in the message and
// the data is empty. That's literally what Lark documents. We test it
// matches their sample.

package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

type larkClient struct {
	url      string
	secret   string
	signOn   bool
	instance string // sending-gateway label, shown in the message header
	attempts int
	httpc    *http.Client
}

func newLarkClient(cfg config.LarkConfig, secret, instance string, retries int) *larkClient {
	if retries <= 0 {
		retries = 3
	}
	return &larkClient{
		url:      cfg.WebhookURL,
		secret:   secret,
		signOn:   cfg.SignatureRequired,
		instance: instance,
		attempts: retries,
		httpc:    &http.Client{Timeout: 8 * time.Second},
	}
}

// Send posts the batch as a single Lark message. Implements retry with
// exponential backoff (1s, 2s, 4s, ...). Returns the last error if all
// attempts fail.
func (c *larkClient) Send(batch []Event) error {
	if c.signOn && c.secret == "" {
		return errors.New("lark: signature required but no secret configured (set via /api/notifications/lark)")
	}
	body, err := c.buildBody(batch)
	if err != nil {
		return err
	}
	var lastErr error
	delay := 1 * time.Second
	for attempt := 0; attempt < c.attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			// Lark returns 200 even when signing fails — must inspect body.
			if !strings.Contains(string(respBody), `"code":0`) &&
				!strings.Contains(string(respBody), `"StatusCode":0`) {
				lastErr = fmt.Errorf("lark response not OK: %s", string(respBody))
				continue
			}
			return nil
		}
		lastErr = fmt.Errorf("lark http %d: %s", resp.StatusCode, string(respBody))
	}
	if lastErr == nil {
		lastErr = errors.New("lark: all attempts exhausted (no specific error)")
	}
	return lastErr
}

// buildBody renders one or more events as a single Lark text message.
// We use msg_type=text (the simplest, most-broadly-supported format)
// rather than rich-card. Events render as:
//
//   [URGENT] node X is dead (fail_rate=100% for 30min)
//
//     primary: ash/IEPL-01  (rtt_p95=251ms, fail_rate=0.00)
//     ...
//
// When multiple events batch, they're listed sequentially with a blank
// line between.
func (c *larkClient) buildBody(batch []Event) ([]byte, error) {
	ts := time.Now().Unix()
	content := renderBatch(batch, c.instance)
	payload := map[string]any{
		"timestamp": fmt.Sprintf("%d", ts),
		"msg_type":  "text",
		"content":   map[string]any{"text": content},
	}
	if c.signOn {
		sig, err := signLark(ts, c.secret)
		if err != nil {
			return nil, err
		}
		payload["sign"] = sig
	}
	return json.Marshal(payload)
}

// signLark computes the HMAC-SHA256 signature Lark expects.
func signLark(timestamp int64, secret string) (string, error) {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	mac := hmac.New(sha256.New, []byte(stringToSign))
	if _, err := mac.Write([]byte("")); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// renderBatch turns a list of events into the plain-text body Lark
// renders inline. Header carries the highest severity in the batch +
// count of events. instance identifies the sending gateway (e.g. "92")
// so an operator watching one channel for both 89 and 92 can tell which
// node fired the alert; empty instance falls back to a bare label.
func renderBatch(batch []Event, instance string) string {
	if len(batch) == 0 {
		return ""
	}
	maxSev := SeverityInfo
	for _, ev := range batch {
		if ev.Severity == SeverityUrgent {
			maxSev = SeverityUrgent
			break
		}
	}
	label := "leap-gateway"
	if instance != "" {
		label = "leap-gateway@" + instance
	}
	var b strings.Builder
	switch maxSev {
	case SeverityUrgent:
		fmt.Fprintf(&b, "🚨 %s URGENT", label)
	case SeverityDone:
		fmt.Fprintf(&b, "✅ %s", label)
	default:
		fmt.Fprintf(&b, "ℹ️ %s", label)
	}
	if len(batch) > 1 {
		fmt.Fprintf(&b, " — %d events\n\n", len(batch))
	} else {
		b.WriteString("\n\n")
	}
	for i, ev := range batch {
		if i > 0 {
			b.WriteString("\n──────────\n")
		}
		fmt.Fprintf(&b, "[%s] %s\n", strings.ToUpper(string(ev.Severity)), ev.Subject)
		if ev.Node != "" {
			fmt.Fprintf(&b, "node: %s\n", ev.Node)
		}
		if ev.Body != "" {
			fmt.Fprintf(&b, "\n%s\n", ev.Body)
		}
		fmt.Fprintf(&b, "\nat: %s", ev.Time.Format(time.RFC3339))
	}
	return b.String()
}
