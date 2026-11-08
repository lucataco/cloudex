package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Token is an access token: an API key other than the admin key, with a role.
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // token, or session for the dashboard
	Role       string `json:"role"` // admin, user or read
	OwnOnly    bool   `json:"ownOnly"`
	CreatedAt  int64  `json:"createdAt"`
	CreatedBy  string `json:"createdBy"`
	ExpiresAt  *int64 `json:"expiresAt"`
	LastUsedAt *int64 `json:"lastUsedAt"`
	// Parent is, for a dashboard session signed in with a token, that token.
	Parent *string `json:"parent,omitempty"`
}

// NewToken is a token as made, with its secret, which nothing shows again.
type NewToken struct {
	Token
	Secret string `json:"secret"`
}

type CreateTokenRequest struct {
	Name         string `json:"name"`
	Role         string `json:"role,omitempty"`
	OwnOnly      bool   `json:"ownOnly,omitempty"`
	ExpiresInSec int64  `json:"expiresInSec,omitempty"`
}

// Me is who the caller is; Token is nil for the admin key.
type Me struct {
	Role    string `json:"role"`
	OwnOnly bool   `json:"ownOnly"`
	Token   *Token `json:"token"`
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var m Me
	return &m, c.do(ctx, http.MethodGet, "/v1/me", nil, &m)
}

func (c *Client) CreateToken(ctx context.Context, r CreateTokenRequest) (*NewToken, error) {
	var t NewToken
	return &t, c.do(ctx, http.MethodPost, "/v1/tokens", r, &t)
}

func (c *Client) ListTokens(ctx context.Context) ([]Token, error) {
	var ts []Token
	return ts, c.do(ctx, http.MethodGet, "/v1/tokens", nil, &ts)
}

func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/tokens/"+url.PathEscape(id), nil, nil)
}

// DeviceCode is the start of a device login (RFC 8628).
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type DeviceToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Token       Token  `json:"token"`
}

// DeviceRequest is a device login waiting for approval.
type DeviceRequest struct {
	UserCode  string  `json:"userCode"`
	Name      string  `json:"name"`
	Role      *string `json:"role"`
	IP        *string `json:"ip"`
	Country   *string `json:"country"`
	UserAgent *string `json:"userAgent"`
	CreatedAt int64   `json:"createdAt"`
	ExpiresAt int64   `json:"expiresAt"`
}

type DeviceApproveRequest struct {
	UserCode     string `json:"user_code"`
	Role         string `json:"role,omitempty"`
	OwnOnly      bool   `json:"ownOnly,omitempty"`
	ExpiresInSec int64  `json:"expiresInSec,omitempty"`
}

func (c *Client) StartDeviceLogin(ctx context.Context, name, role string) (*DeviceCode, error) {
	body := map[string]string{"name": name}
	if role != "" {
		body["role"] = role
	}
	var d DeviceCode
	return &d, c.do(ctx, http.MethodPost, "/v1/auth/device", body, &d)
}

// ErrPending and ErrSlowDown: no decision yet; ask again after the interval,
// or after a longer one.
var (
	ErrPending  = errors.New("authorization_pending")
	ErrSlowDown = errors.New("slow_down")
)

// PollDeviceLogin answers the token once the login is approved, ErrPending or
// ErrSlowDown before that, and an *Error saying access_denied or expired_token
// when it's over.
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*DeviceToken, error) {
	var t DeviceToken
	err := c.do(ctx, http.MethodPost, "/v1/auth/token", map[string]string{"device_code": deviceCode}, &t)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusBadRequest {
		switch e.Message {
		case "authorization_pending":
			return nil, ErrPending
		case "slow_down":
			return nil, ErrSlowDown
		}
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) ListDeviceLogins(ctx context.Context) ([]DeviceRequest, error) {
	var rs []DeviceRequest
	return rs, c.do(ctx, http.MethodGet, "/v1/auth/device", nil, &rs)
}

func (c *Client) ApproveDeviceLogin(ctx context.Context, r DeviceApproveRequest) (*DeviceRequest, error) {
	var d DeviceRequest
	return &d, c.do(ctx, http.MethodPost, "/v1/auth/device/approve", r, &d)
}

func (c *Client) DenyDeviceLogin(ctx context.Context, userCode string) (*DeviceRequest, error) {
	var d DeviceRequest
	return &d, c.do(ctx, http.MethodPost, "/v1/auth/device/deny", map[string]string{"user_code": userCode}, &d)
}

// LoginLink is a one-time link that signs a browser in to the dashboard.
type LoginLink struct {
	URL       string `json:"url"`
	ExpiresIn int    `json:"expiresIn"`
}

// LoginLink makes a link to the dashboard at path (such as /s/web) that signs
// a browser in as this client, once, within ExpiresIn seconds.
func (c *Client) LoginLink(ctx context.Context, path string) (*LoginLink, error) {
	var l LoginLink
	return &l, c.do(ctx, http.MethodPost, "/v1/auth/links", map[string]string{"path": path}, &l)
}
