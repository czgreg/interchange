// notifications.go — operator-facing API for the notify subsystem.
//
// Five endpoints:
//   GET    /api/notifications/status       — config snapshot (no secret)
//   POST   /api/notifications/lark         — set webhook url + secret
//   DELETE /api/notifications/lark         — clear webhook url + secret
//   POST   /api/notifications/test         — send a synthetic test message
//   GET    /api/notifications/recent       — last N entries from local log

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

func (s *Server) handleNotificationsStatus(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "notifier not initialized",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Notifier.GetStatus())
}

// larkConfigDTO is the request body for POST /api/notifications/lark.
// secret is required when signature_required=true (the recommended
// production setup); webhook_url is always required.
type larkConfigDTO struct {
	WebhookURL        string `json:"webhook_url"`
	Secret            string `json:"secret,omitempty"`
	SignatureRequired *bool  `json:"signature_required,omitempty"` // pointer to disambiguate "missing" from "false"
}

func (s *Server) handleNotificationsLark(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notifier not initialized"})
		return
	}
	var dto larkConfigDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if dto.WebhookURL == "" {
		http.Error(w, "webhook_url required", http.StatusBadRequest)
		return
	}
	sigRequired := true // default ON for safety
	if dto.SignatureRequired != nil {
		sigRequired = *dto.SignatureRequired
	}
	if sigRequired && dto.Secret == "" {
		http.Error(w, "secret required when signature_required=true (default)", http.StatusBadRequest)
		return
	}

	// Persist to disk: secret file (chmod 0600), then yaml (URL only).
	secretPath := s.deps.Cfg.Notifications.Lark.SecretFile
	if secretPath == "" {
		secretPath = "/var/lib/leap/lark-secret"
	}
	if err := writeLarkSecret(secretPath, dto.Secret); err != nil {
		http.Error(w, "persist secret: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Notifications.Enabled = true
		c.Notifications.Lark.WebhookURL = dto.WebhookURL
		c.Notifications.Lark.SignatureRequired = sigRequired
		return nil
	}); err != nil {
		http.Error(w, "persist config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	status := s.deps.Notifier.Configure(dto.WebhookURL, dto.Secret, sigRequired)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleNotificationsLarkDelete(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notifier not initialized"})
		return
	}
	secretPath := s.deps.Cfg.Notifications.Lark.SecretFile
	if secretPath == "" {
		secretPath = "/var/lib/leap/lark-secret"
	}
	_ = os.Remove(secretPath) // ok if already absent
	if err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Notifications.Lark.WebhookURL = ""
		return nil
	}); err != nil {
		http.Error(w, "persist config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	status := s.deps.Notifier.Configure("", "", true)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleNotificationsTest(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notifier not initialized"})
		return
	}
	if err := s.deps.Notifier.TestSend(); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleNotificationsRecent(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notifier not initialized"})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := s.deps.Notifier.Recent(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "limit": limit})
}

// writeLarkSecret persists the secret to disk with 0600 permissions.
// Atomic via tmp-file + rename so a partial write can't leave a
// corrupted secret.
func writeLarkSecret(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(secret), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
