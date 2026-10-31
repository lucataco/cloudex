// Package cloudflare covers the Cloudflare API calls `cloudex deploy` makes:
// the account's resources, its container registry, and uploading the API Worker
// with its container application, the same calls wrangler deploy makes.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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
	return NewAt(token, apiBase)
}

// NewAt talks to another API base URL, such as a test server.
func NewAt(token, base string) *Client {
	return &Client{token: token, base: base, http: &http.Client{}}
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
	return c.doHeader(ctx, method, path, nil, body, out)
}

func (c *Client) doHeader(ctx context.Context, method, path string, header http.Header, body, out any) error {
	var rd io.Reader
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd, contentType = bytes.NewReader(data), "application/json"
	}
	return c.send(ctx, method, path, header, contentType, rd, out)
}

func (c *Client) send(ctx context.Context, method, path string, header http.Header, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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

// EnsureR2Bucket finds or creates an R2 bucket. An account that hasn't turned
// R2 on in the dashboard gets code 10042; a token without R2 access gets a 403.
func (c *Client) EnsureR2Bucket(ctx context.Context, account, name string) (created bool, err error) {
	err = c.do(ctx, http.MethodGet, "/accounts/"+account+"/r2/buckets/"+url.PathEscape(name), nil, nil)
	var cfErr *Error
	if err == nil || !errors.As(err, &cfErr) || cfErr.Status != http.StatusNotFound {
		return false, err
	}
	return true, c.do(ctx, http.MethodPost, "/accounts/"+account+"/r2/buckets", map[string]string{"name": name}, nil)
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

// PrepareImage asks Cloudflare to get a digest-pinned image in the account's
// registry ready to run. The status is "ready", "pending" (ask again) or
// "error", with a reason.
func (c *Client) PrepareImage(ctx context.Context, account, image string) (status, reason string, err error) {
	var out struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	err = c.do(ctx, http.MethodPost, "/accounts/"+account+"/containers/image-preparations", map[string]string{"image": image}, &out)
	return out.Status, out.Reason, err
}

// UploadWorker uploads a module Worker and deploys it to all its traffic. The
// metadata is what the Workers script upload API takes: main_module, bindings,
// compatibility date and flags, exports and containers.
func (c *Client) UploadWorker(ctx context.Context, account, name string, metadata any, module string, code []byte) error {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("metadata", string(meta)); err != nil {
		return err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, module, module))
	h.Set("Content-Type", "application/javascript+module")
	part, err := form.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(code); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	path := "/accounts/" + account + "/workers/scripts/" + url.PathEscape(name) + "?bindings_inherit=strict"
	return c.send(ctx, http.MethodPut, path, nil, form.FormDataContentType(), &body, nil)
}

// DurableObjectNamespace finds the namespace of a Worker's Durable Object
// class among its bindings.
func (c *Client) DurableObjectNamespace(ctx context.Context, account, script, class string) (string, error) {
	var out struct {
		Bindings []struct {
			Type        string `json:"type"`
			ClassName   string `json:"class_name"`
			ScriptName  string `json:"script_name"`
			NamespaceID string `json:"namespace_id"`
		} `json:"bindings"`
	}
	if err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/workers/scripts/"+url.PathEscape(script)+"/settings", nil, &out); err != nil {
		return "", err
	}
	for _, b := range out.Bindings {
		if b.Type == "durable_object_namespace" && b.ClassName == class && (b.ScriptName == "" || b.ScriptName == script) && b.NamespaceID != "" {
			return b.NamespaceID, nil
		}
	}
	return "", fmt.Errorf("the Worker %s has no Durable Object namespace for %s", script, class)
}

// EnsureContainerApplication gives a Durable Object namespace its container
// application, which lets its objects start containers from the images in the
// Worker's metadata. The application's id is the namespace's.
func (c *Client) EnsureContainerApplication(ctx context.Context, account, name, namespaceID string) (created bool, err error) {
	var app struct {
		Name             string `json:"name"`
		SchedulingPolicy string `json:"scheduling_policy"`
	}
	err = c.do(ctx, http.MethodGet, "/accounts/"+account+"/containers/applications/"+url.PathEscape(namespaceID), nil, &app)
	var cfErr *Error
	switch {
	case err == nil && (app.Name != name || app.SchedulingPolicy != "durable_object"):
		return false, fmt.Errorf("the container application for the Durable Objects is %s (%s), not %s; delete it in the Cloudflare dashboard and deploy again", app.Name, app.SchedulingPolicy, name)
	case err == nil:
		return false, nil
	case !errors.As(err, &cfErr) || cfErr.Status != http.StatusNotFound:
		return false, err
	}
	body := map[string]any{
		"name":              name,
		"scheduling_policy": "durable_object",
		"durable_objects":   map[string]string{"namespace_id": namespaceID},
	}
	return true, c.do(ctx, http.MethodPost, "/accounts/"+account+"/containers/applications", body, nil)
}

// EnableWorkersDev serves the Worker at <script>.<subdomain>.workers.dev.
func (c *Client) EnableWorkersDev(ctx context.Context, account, script string) error {
	header := http.Header{"Cloudflare-Workers-Script-Api-Date": {"2025-08-01"}}
	return c.doHeader(ctx, http.MethodPost, "/accounts/"+account+"/workers/scripts/"+url.PathEscape(script)+"/subdomain", header, map[string]bool{"enabled": true}, nil)
}

// RouteWorker sends a route pattern in a zone to the Worker, replacing its
// other routes. A token limited to some zones can't use that account-wide
// call; then the route is added through the zone, as wrangler does.
func (c *Client) RouteWorker(ctx context.Context, account, script, zoneID, zone, pattern string) error {
	routes := []map[string]string{{"pattern": pattern, "zone_name": zone}}
	err := c.do(ctx, http.MethodPut, "/accounts/"+account+"/workers/scripts/"+url.PathEscape(script)+"/routes", routes, nil)
	var cfErr *Error
	if err == nil || !errors.As(err, &cfErr) || (cfErr.Status != http.StatusForbidden && cfErr.Code != 10000) {
		return err
	}
	var existing []struct {
		Pattern string `json:"pattern"`
		Script  string `json:"script"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/workers/routes", nil, &existing); err != nil {
		return err
	}
	for _, r := range existing {
		if r.Pattern != pattern {
			continue
		}
		if r.Script == script {
			return nil
		}
		return &privateError{
			msg:    fmt.Sprintf("the route %s already goes to the Worker %s; remove it in the Cloudflare dashboard and deploy again", pattern, r.Script),
			values: []string{pattern, r.Script},
		}
	}
	return c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/workers/routes", map[string]string{"pattern": pattern, "script": script}, nil)
}

// privateError names things in the account, such as another Worker, that
// cloudex feedback leaves out of a report.
type privateError struct {
	msg    string
	values []string
}

func (e *privateError) Error() string           { return e.msg }
func (e *privateError) PrivateValues() []string { return e.values }

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
