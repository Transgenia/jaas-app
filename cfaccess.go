package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CFAccessVerifier validates the Cf-Access-Jwt-Assertion header that
// Cloudflare Access adds after its own login. Checking it in the app closes
// the gap where someone reaches the origin directly and skips Access.
type CFAccessVerifier struct {
	TeamDomain    string // e.g. transgenia.cloudflareaccess.com
	AUD           string // Application Audience (AUD) tag
	AllowedEmails []string
	HTTP          *http.Client
	CertsURL      string // override for tests; defaults to the team's certs endpoint

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func NewCFAccessVerifier(cfg *Config) *CFAccessVerifier {
	if cfg.CFAccessTeamDomain == "" {
		return nil
	}
	return &CFAccessVerifier{
		TeamDomain:    strings.TrimPrefix(strings.TrimSuffix(cfg.CFAccessTeamDomain, "/"), "https://"),
		AUD:           cfg.CFAccessAUD,
		AllowedEmails: cfg.CFAccessAllowedEmails,
		HTTP:          &http.Client{Timeout: 10 * time.Second},
	}
}

func (v *CFAccessVerifier) issuer() string { return "https://" + v.TeamDomain }

func (v *CFAccessVerifier) refresh(ctx context.Context) error {
	certsURL := v.CertsURL
	if certsURL == "" {
		certsURL = v.issuer() + "/cdn-cgi/access/certs"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certsURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return errors.New("cf access: certs fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cf access: certs HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return errors.New("cf access: certs decode failed")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("cf access: no RSA keys")
	}
	v.keys, v.fetchedAt = keys, time.Now()
	return nil
}

func (v *CFAccessVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < 24*time.Hour {
		return k, nil
	}
	// Unknown kid (key rotation) or stale cache: refetch at most once a minute.
	if time.Since(v.fetchedAt) > time.Minute || v.keys == nil {
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("cf access: unknown key id")
}

func (v *CFAccessVerifier) Verify(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("cf access: missing assertion")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(v.AUD),
		jwt.WithIssuer(v.issuer()),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return errors.New("cf access: invalid assertion")
	}
	if len(v.AllowedEmails) > 0 {
		email, _ := claims["email"].(string)
		if !slices.Contains(v.AllowedEmails, strings.ToLower(email)) {
			return errors.New("cf access: email not allowed")
		}
	}
	return nil
}
