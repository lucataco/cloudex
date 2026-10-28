// Package cloudflare covers the few Cloudflare API calls `cloudex deploy` makes
// itself. Uploading the Worker and its container application is left to wrangler.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const apiBase = "https://api.cloudflare.com/client/v4"

type Client struct {
	token string
	base  string
	http  *http.Client
}

func New(token string) *Client {
	return &Client{token: token, base: apiBase, http: &http.Client{}}
}

// Error carries Cloudflare's own message, which usually explains the fix.
type Error struct {
	Status  int
	Code    int
	Message string
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("Cloudflare API: %s (code %d)", e.Message, e.Code)
	}
	return "Cloudflare API: " + e.Message
}

type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the Cloudflare API: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return &Error{Status: res.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if !env.Success || res.StatusCode >= 300 {
		e := &Error{Status: res.StatusCode, Message: res.Status}
		if len(env.Errors) > 0 {
			e.Code, e.Message = env.Errors[0].Code, unwrap(env.Errors[0].Message)
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// Some APIs (Containers) put a JSON object in the error message.
func unwrap(msg string) string {
	var inner struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &inner) == nil && inner.Error != "" {
		return inner.Error
	}
	return msg
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the accounts the token can reach. It also proves the token works.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	return out, c.do(ctx, http.MethodGet, "/accounts?per_page=50", nil, &out)
}

// CheckContainers fails with Cloudflare's explanation when the account can't
// run containers, which today means it isn't on Workers Paid.
func (c *Client) CheckContainers(ctx context.Context, account string) error {
	return c.do(ctx, http.MethodGet, "/accounts/"+account+"/containers/applications", nil, nil)
}

// WorkersSubdomain is the account's <name>.workers.dev subdomain, or "" if it
// has never chosen one.
func (c *Client) WorkersSubdomain(ctx context.Context, account string) (string, error) {
	var out struct {
		Subdomain string `json:"subdomain"`
	}
	err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/workers/subdomain", nil, &out)
	var e *Error
	if asError(err, &e) && e.Status == http.StatusNotFound {
		return "", nil
	}
	return out.Subdomain, err
}

// EnsureD1 returns the id of the named database, creating it if needed.
func (c *Client) EnsureD1(ctx context.Context, account, name string) (id string, created bool, err error) {
	var list []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/d1/database?name="+url.QueryEscape(name), nil, &list); err != nil {
		return "", false, err
	}
	for _, db := range list {
		if db.Name == name {
			return db.UUID, false, nil
		}
	}
	var db struct {
		UUID string `json:"uuid"`
	}
	if err := c.do(ctx, http.MethodPost, "/accounts/"+account+"/d1/database", map[string]string{"name": name}, &db); err != nil {
		return "", false, err
	}
	return db.UUID, true, nil
}

// RegistryCredentials returns short-lived push and pull credentials for the
// account's registry at registry.cloudflare.com.
func (c *Client) RegistryCredentials(ctx context.Context, account string, minutes int) (username, password string, err error) {
	var out struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	body := map[string]any{"expiration_minutes": minutes, "permissions": []string{"pull", "push"}}
	err = c.do(ctx, http.MethodPost, "/accounts/"+account+"/containers/registries/registry.cloudflare.com/credentials", body, &out)
	return out.Username, out.Password, err
}

// ZoneID finds a zone by name within the account.
func (c *Client) ZoneID(ctx context.Context, account, name string) (string, error) {
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	q := url.Values{"name": {name}, "account.id": {account}}
	if err := c.do(ctx, http.MethodGet, "/zones?"+q.Encode(), nil, &zones); err != nil {
		return "", err
	}
	for _, z := range zones {
		if z.Name == name {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("%s isn't a zone in this Cloudflare account; add the domain to the account first", name)
}

// EnsureWildcardRecord makes *.zone resolve through Cloudflare so the Worker
// route can answer preview hosts. 100:: is the usual placeholder for a
// proxied record whose only job is to reach a Worker.
func (c *Client) EnsureWildcardRecord(ctx context.Context, zoneID, zone string) (created bool, err error) {
	var records []struct {
		ID string `json:"id"`
	}
	q := url.Values{"name": {"*." + zone}}
	if err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &records); err != nil {
		return false, err
	}
	if len(records) > 0 {
		return false, nil
	}
	body := map[string]any{"type": "AAAA", "name": "*." + zone, "content": "100::", "proxied": true, "comment": "Cloudex preview links"}
	return true, c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, nil)
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
