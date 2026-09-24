// Package manufacturer owns the local OAuth/discovery boundary, not equipment control.
package manufacturer

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"thermora.local/thermora/internal/identity"
	"thermora.local/thermora/internal/messaging"
)

const ClientID = "thermora-cloud"

var ErrAuthorization = errors.New("manufacturer authorization required")

type Config struct {
	APIURL, AuthorizeURL, RedirectURL, ClientSecret, EncryptionKey, CAFile string
	OrganizationID                                                         string
	Broker                                                                 messaging.Settings
}

type Client struct {
	Config Config
	HTTP   *http.Client
	seal   cipher.AEAD
}

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Discovery struct {
	Subject string   `json:"subject"`
	Devices []Device `json:"devices"`
}

func NewClient(cfg Config) (*Client, error) {
	for _, raw := range []string{cfg.APIURL, cfg.AuthorizeURL, cfg.RedirectURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("manufacturer endpoints require fixed HTTPS URLs without credentials or queries")
		}
	}
	key, err := base64.RawStdEncoding.DecodeString(cfg.EncryptionKey)
	if err != nil || len(key) != 32 || len(cfg.ClientSecret) < 32 {
		return nil, errors.New("invalid local manufacturer encryption key or client secret")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	tls, err := messaging.TLSConfig(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	return &Client{Config: cfg, seal: aead, HTTP: &http.Client{
		Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: tls},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Encrypt(data []byte, binding string) []byte {
	return c.seal.Seal(nil, nil, data, []byte(binding))
}

func (c *Client) Decrypt(data []byte, binding string) ([]byte, error) {
	return c.seal.Open(nil, nil, data, []byte(binding))
}

func Challenge(verifier string) string {
	return base64.RawURLEncoding.EncodeToString(identity.Digest(verifier))
}

func (c *Client) AuthorizationURL(state, verifier string) string {
	return c.Config.AuthorizeURL + "?" + url.Values{
		"client_id": {ClientID}, "redirect_uri": {c.Config.RedirectURL},
		"response_type": {"code"}, "scope": {"devices:read devices:control"}, "state": {state},
		"code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"},
	}.Encode()
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (Tokens, error) {
	return c.token(
		ctx,
		url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {verifier},
			"redirect_uri":  {c.Config.RedirectURL},
		},
	)
}

func (c *Client) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	return c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

func (c *Client) token(ctx context.Context, values url.Values) (Tokens, error) {
	var result Tokens
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.Config.APIURL+"/oauth/token",
		strings.NewReader(values.Encode()),
	)
	if err != nil {
		return result, err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(ClientID, c.Config.ClientSecret)
	if err = c.read(r, &result); err != nil {
		return result, err
	}
	if result.TokenType != "Bearer" || len(result.AccessToken) < 32 || len(result.RefreshToken) < 32 ||
		result.ExpiresIn < 1 ||
		result.ExpiresIn > 86400 {
		return result, errors.New("manufacturer returned invalid token metadata")
	}
	return result, nil
}

func (c *Client) Discover(ctx context.Context, token string) (Discovery, error) {
	var result Discovery
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Config.APIURL+"/devices", nil)
	if err != nil {
		return result, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if err = c.read(r, &result); err != nil {
		return result, err
	}
	if result.Subject == "" || len(result.Subject) > 100 || len(result.Devices) > 25 {
		return result, errors.New("invalid manufacturer discovery")
	}
	seen := make(map[string]bool, len(result.Devices))
	for _, d := range result.Devices {
		if d.ID == "" || len(d.ID) > 100 || d.Name == "" || len(d.Name) > 100 || seen[d.ID] {
			return result, errors.New("invalid manufacturer device")
		}
		seen[d.ID] = true
	}
	return result, nil
}

func (c *Client) read(r *http.Request, out any) (err error) {
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return fmt.Errorf("manufacturer request failed: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode == 400 || resp.StatusCode == 401 {
		return ErrAuthorization
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("manufacturer unavailable: HTTP %d", resp.StatusCode)
	}
	limit := 65536
	if strings.HasSuffix(r.URL.Path, "/state") {
		limit = 768 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit+1)))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("manufacturer response too large")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	r, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.Config.APIURL+"/oauth/revoke",
		strings.NewReader(url.Values{"token": {token}}.Encode()),
	)
	if err != nil {
		return err
	}
	r.SetBasicAuth(ClientID, c.Config.ClientSecret)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.read(r, nil)
}
