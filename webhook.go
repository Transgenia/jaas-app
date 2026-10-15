package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// --- signature (https://developer.8x8.com/jaas/docs/webhooks-signatures/) ---

var errBadSignature = errors.New("invalid webhook signature")

// verifyJaaSSignature checks X-Jaas-Signature: "t=<unix>,v1=<base64>[,v1=...]".
// The signed payload is "<t>.<raw body>", HMAC-SHA256 with the endpoint
// secret, base64 encoded. Schemes other than v1 are ignored (downgrade
// protection) and the timestamp must be within tolerance (replay protection).
func verifyJaaSSignature(header string, body, secret []byte, tolerance time.Duration, now time.Time) error {
	if header == "" || len(secret) == 0 {
		return errBadSignature
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return errBadSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", errBadSignature)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(expected)) {
			return nil
		}
	}
	return errBadSignature
}

// --- download URL policy (SSRF) ---

var errURLNotAllowed = errors.New("download URL not allowed")

// hostMatches supports exact hosts and "*" as exactly one DNS label, e.g.
// "objectstorage.*.oraclecloud.com".
func hostMatches(host, pattern string) bool {
	hl := strings.Split(strings.ToLower(host), ".")
	pl := strings.Split(strings.ToLower(pattern), ".")
	if len(hl) != len(pl) {
		return false
	}
	for i := range pl {
		if pl[i] != "*" && pl[i] != hl[i] {
			return false
		}
		if hl[i] == "" {
			return false
		}
	}
	return true
}

func validateDownloadURL(raw string, allowed []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return nil, errURLNotAllowed
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, errURLNotAllowed
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return nil, errURLNotAllowed
	}
	for _, pat := range allowed {
		if hostMatches(host, pat) {
			return u, nil
		}
	}
	return nil, errURLNotAllowed
}

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b::/96",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// newDownloadClient refuses to connect to non-public addresses (even if DNS
// for an allowed host is poisoned), revalidates every redirect against the
// allow list, and bounds every phase of the request.
func newDownloadClient(allowed []string) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil || !isPublicIP(net.ParseIP(host)) {
				return errURLNotAllowed
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  30 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			ForceAttemptHTTP2:      true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errURLNotAllowed
			}
			_, err := validateDownloadURL(req.URL.String(), allowed)
			return err
		},
	}
}

// downloadLimited fetches an already-validated URL and caps the body size.
// Errors never include the URL: pre-authenticated links are credentials.
func downloadLimited(ctx context.Context, client *http.Client, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("download: invalid request")
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errURLNotAllowed) {
			return nil, errors.New("download: redirect or address not allowed")
		}
		return nil, errors.New("download: connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Op: "download", Code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, errors.New("download: read failed")
	}
	if int64(len(body)) > max {
		return nil, errors.New("download: file exceeds size limit")
	}
	return body, nil
}

type httpStatusError struct {
	Op   string
	Code int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.Op, e.Code) }

// Permanent client errors will not heal by retrying the same request.
func (e *httpStatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != 408 && e.Code != 409 && e.Code != 429
}

// --- webhook handler ---

type jaasWebhook struct {
	IdempotencyKey string `json:"idempotencyKey"`
	AppID          string `json:"appId"`
	EventType      string `json:"eventType"`
	SessionID      string `json:"sessionId"`
	Timestamp      int64  `json:"timestamp"`
	FQN            string `json:"fqn"`
	Data           struct {
		PreAuthenticatedLink string `json:"preAuthenticatedLink"`
		DurationSec          int    `json:"durationSec"`
		LinkExpiration       int64  `json:"linkExpiration"`
		Name                 string `json:"name"`
		Email                string `json:"email"`
		Moderator            bool   `json:"moderator"`
	} `json:"data"`
}

// handleRecordingWebhook accepts JaaS events. It answers 2xx only once the
// event is durably stored, so JaaS retries on our failures; duplicates are
// dropped by idempotencyKey and minutes are unique per (room, session).
func (app *App) handleRecordingWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := verifyJaaSSignature(r.Header.Get("X-Jaas-Signature"), body, app.Config.WebhookSecret,
		app.Config.WebhookTolerance, app.now()); err != nil {
		slog.Warn("webhook: rejected", "reason", "signature", "ip", app.clientIP(r))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var ev jaasWebhook
	if err := json.Unmarshal(body, &ev); err != nil || ev.IdempotencyKey == "" || ev.EventType == "" {
		slog.Warn("webhook: rejected", "reason", "payload")
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	appID, slug, ok := strings.Cut(ev.FQN, "/")
	if !ok || appID != app.Config.JaaSAppID || (ev.AppID != "" && ev.AppID != app.Config.JaaSAppID) {
		slog.Warn("webhook: ignored", "reason", "foreign app", "event", ev.EventType)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !validSlug(slug) {
		slog.Warn("webhook: ignored", "reason", "invalid room", "event", ev.EventType)
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := app.DB.GetRoom(slug); err != nil {
		if errors.Is(err, ErrNotFound) {
			slog.Info("webhook: ignored", "reason", "unknown room", "event", ev.EventType)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	fresh, err := app.DB.RecordWebhookEvent(ev.IdempotencyKey, ev.EventType, slug, ev.SessionID)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !fresh {
		slog.Info("webhook: duplicate", "event", ev.EventType, "room", slug)
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := app.applyWebhook(&ev, slug); err != nil {
		// Let JaaS retry this delivery: forget the key so the retry is not
		// mistaken for a duplicate.
		app.DB.ForgetWebhookEvent(ev.IdempotencyKey)
		var rejected *rejectedEvent
		if errors.As(err, &rejected) {
			slog.Warn("webhook: rejected", "reason", rejected.reason, "event", ev.EventType, "room", slug)
			http.Error(w, "Unprocessable Entity", http.StatusUnprocessableEntity)
			return
		}
		slog.Error("webhook: store failed", "event", ev.EventType, "room", slug)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type rejectedEvent struct{ reason string }

func (e *rejectedEvent) Error() string { return e.reason }

// rejectedLink names only the host of a refused link (never path or query,
// which carry the pre-authentication) so the allow list can be adjusted.
func rejectedLink(raw string) *rejectedEvent {
	host := "unparseable"
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host = u.Scheme + "://" + u.Hostname()
	}
	return &rejectedEvent{"link not allowed: " + host}
}

func (app *App) linkExpiry(ev *jaasWebhook) time.Time {
	// JaaS documents pre-authenticated links as valid for 24 hours.
	if ev.Data.LinkExpiration > 0 {
		return time.UnixMilli(ev.Data.LinkExpiration)
	}
	return app.now().Add(24 * time.Hour)
}

func (app *App) applyWebhook(ev *jaasWebhook, slug string) error {
	switch ev.EventType {
	case "TRANSCRIPTION_UPLOADED":
		if ev.SessionID == "" {
			return &rejectedEvent{"missing sessionId"}
		}
		u, err := validateDownloadURL(ev.Data.PreAuthenticatedLink, app.Config.DownloadAllowedHosts)
		if err != nil {
			return rejectedLink(ev.Data.PreAuthenticatedLink)
		}
		meetingAt := app.now()
		if ev.Timestamp > 0 {
			meetingAt = time.UnixMilli(ev.Timestamp)
		}
		created, err := app.DB.EnqueueMinutes(slug, ev.SessionID, u.String(), app.linkExpiry(ev), meetingAt)
		if err != nil {
			return err
		}
		slog.Info("webhook: transcription", "room", slug, "session", ev.SessionID, "queued", created)
		app.wakeWorker()
		return nil

	case "RECORDING_UPLOADED":
		u, err := validateDownloadURL(ev.Data.PreAuthenticatedLink, app.Config.DownloadAllowedHosts)
		if err != nil {
			return rejectedLink(ev.Data.PreAuthenticatedLink)
		}
		if err := app.DB.AddRecording(slug, ev.SessionID, u.String(), app.linkExpiry(ev), ev.Data.DurationSec); err != nil {
			return err
		}
		slog.Info("webhook: recording", "room", slug, "session", ev.SessionID)
		return nil

	case "PARTICIPANT_JOINED":
		// The e-mail is the one we put in the participant's JWT.
		email := normalizeEmail(ev.Data.Email)
		if email == "" || ev.SessionID == "" {
			slog.Info("webhook: participant without e-mail", "room", slug)
			return nil
		}
		at := app.now()
		if ev.Timestamp > 0 {
			at = time.UnixMilli(ev.Timestamp)
		}
		if err := app.DB.RecordJaaSParticipant(slug, ev.SessionID, email, cleanDisplayName(ev.Data.Name), ev.Data.Moderator, at); err != nil {
			return err
		}
		slog.Info("webhook: participant", "room", slug, "session", ev.SessionID)
		return nil

	default:
		slog.Info("webhook: event", "event", ev.EventType, "room", slug)
		return nil
	}
}
