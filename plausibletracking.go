// Package caddy_plausible_plugin provides a Caddy HTTP middleware that tracks
// page views server-side by forwarding events to a Plausible Analytics instance.
//
// The middleware sits in the handler chain, delegates the request to the next
// handler, and — after a response has been committed to the client — fires an
// asynchronous pageview event so that the client is never kept waiting.
//
// Static assets (CSS, JS, images, fonts, …) and error responses (4xx/5xx) are
// silently skipped and never forwarded to Plausible.
//
// Supported custom props (opt-in via the `props` directive):
//   - content_type  MIME type of the response (e.g. "text/html", "application/json")
//   - device_type   Coarse device class derived from User-Agent: mobile, tablet, desktop, or bot
//
// Example Caddyfile usage:
//
//	example.com {
//	    plausible {
//	        domain_name example.com
//	        base_url    https://plausible.io   # optional; this is the default
//	        props       content_type device_type
//	    }
//	    file_server
//	}
package caddy_plausible_plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

const DefaultBaseUrl = "https://plausible.io"

// Supported prop names for the `props` Caddyfile directive.
const (
	PropContentType = "content_type"
	PropDeviceType  = "device_type"
)

var regexStaticAssets *regexp.Regexp

func init() {
	regexStaticAssets = regexp.MustCompile(`\.(css|js|png|jpg|jpeg|gif|svg|webp|ico|bmp|tiff|mp3|mp4|avi|mov|webm|ogg|wav|flac|woff|woff2|ttf|map)$`)
	caddy.RegisterModule(PlausiblePlugin{})
	httpcaddyfile.RegisterHandlerDirective("plausible", parseCaddyfile)
}

// PlausiblePlugin is a Caddy middleware that records server-side pageviews in
// Plausible Analytics after each HTTP response is committed.
type PlausiblePlugin struct {
	// BaseURL is the root URL of the Plausible instance. Defaults to
	// https://plausible.io — override this when self-hosting.
	BaseURL string `json:"base_url,omitempty"`

	// DomainName is the site domain as configured in your Plausible dashboard.
	// Required.
	DomainName string `json:"domain_name,omitempty"`

	// Props is the list of custom properties to attach to each event.
	// Supported values: "content_type", "device_type".
	Props []string `json:"props,omitempty"`

	logger *zap.Logger
	client *http.Client
}

// EventPayload is the JSON body sent to the Plausible /api/event endpoint.
type EventPayload struct {
	Name     string            `json:"name"`
	Url      string            `json:"url"`
	Domain   string            `json:"domain"`
	Referrer string            `json:"referrer"`
	Props    map[string]string `json:"props,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (PlausiblePlugin) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.plausible",
		New: func() caddy.Module { return new(PlausiblePlugin) },
	}
}

// Provision sets up the module.
func (m *PlausiblePlugin) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)

	if m.DomainName == "" {
		return errors.New("domain_name is required")
	}
	if m.BaseURL == "" {
		m.BaseURL = DefaultBaseUrl
	}
	m.BaseURL = strings.TrimSuffix(m.BaseURL, "/")

	for _, p := range m.Props {
		if p != PropContentType && p != PropDeviceType {
			return fmt.Errorf("unknown prop %q: supported values are %q and %q", p, PropContentType, PropDeviceType)
		}
	}

	m.client = &http.Client{Timeout: 5 * time.Second}

	return nil
}

// Validate ensures the module configuration is valid.
func (m *PlausiblePlugin) Validate() error {
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
//
// It wraps the ResponseWriter so it can capture the status code and response
// Content-Type, then passes the request down the chain. Once the downstream
// handler returns (and the response has been committed), it records the
// pageview asynchronously.
func (m *PlausiblePlugin) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rw := &responseWriter{ResponseWriter: w}
	// Clone the request before it is passed downstream; subsequent middleware
	// (e.g. php_fastcgi) may mutate it, and we need the original values for
	// the analytics event.
	req := r.Clone(context.TODO())
	if err := next.ServeHTTP(rw, r); err != nil {
		return err
	}
	go m.recordEvent(req, rw.statusCode, rw.contentType)
	return nil
}

// recordEvent builds and ships a pageview to the Plausible /api/event endpoint.
// It is intended to be called inside a goroutine so it never blocks the response.
func (m *PlausiblePlugin) recordEvent(r *http.Request, status int, contentType string) {
	if status >= 400 {
		return // skip error responses
	}

	if regexStaticAssets.MatchString(r.URL.Path) {
		return // skip typical static web assets
	}

	event := EventPayload{
		Name:     "pageview",
		Url:      r.URL.RequestURI(),
		Domain:   m.DomainName,
		Referrer: r.Referer(),
		Props:    m.buildProps(r, contentType),
	}

	eventPayload, err := json.Marshal(event)
	if err != nil {
		m.logger.Error("failed to marshal event json", zap.Error(err))
		return
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/event", m.BaseURL), bytes.NewBuffer(eventPayload))
	if err != nil {
		m.logger.Error("failed to construct request", zap.Error(err))
		return
	}

	req.Header.Set("Content-Type", "application/json")
	// Forward the original visitor's User-Agent and IP so Plausible can
	// perform its own bot filtering and geo-lookup.
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	req.Header.Set("X-Forwarded-For", extractIP(r))

	m.logger.Info("sending plausible event",
		zap.String("domain", event.Domain),
		zap.String("url", event.Url),
	)

	res, err := m.client.Do(req)
	if err != nil {
		m.logger.Error("failed to post plausible event", zap.Error(err))
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		m.logger.Error("failed to post plausible event, got unsuccessful response",
			zap.Int("status_code", res.StatusCode),
		)
	}
}

// buildProps collects the configured custom properties for a single request.
// Only properties listed in m.Props are evaluated; unlisted ones are skipped.
func (m *PlausiblePlugin) buildProps(r *http.Request, contentType string) map[string]string {
	if len(m.Props) == 0 {
		return nil
	}
	props := make(map[string]string, len(m.Props))
	for _, p := range m.Props {
		switch p {
		case PropContentType:
			// Strip charset and other parameters; send only the MIME type.
			mt, _, err := mime.ParseMediaType(contentType)
			if err == nil && mt != "" {
				props[PropContentType] = mt
			}
		case PropDeviceType:
			props[PropDeviceType] = detectDeviceType(r.Header.Get("User-Agent"))
		}
	}
	if len(props) == 0 {
		return nil
	}
	return props
}

// detectDeviceType returns a coarse device class based on the User-Agent string:
// "bot", "tablet", "mobile", or "desktop".
func detectDeviceType(ua string) string {
	lower := strings.ToLower(ua)
	for _, kw := range []string{"bot", "crawler", "spider", "slurp", "bingpreview", "facebookexternalhit", "headlesschrome"} {
		if strings.Contains(lower, kw) {
			return "bot"
		}
	}
	for _, kw := range []string{"tablet", "ipad", "kindle", "silk", "playbook"} {
		if strings.Contains(lower, kw) {
			return "tablet"
		}
	}
	for _, kw := range []string{"mobile", "android", "iphone", "ipod", "windows phone", "blackberry"} {
		if strings.Contains(lower, kw) {
			return "mobile"
		}
	}
	return "desktop"
}

// responseWriter wraps http.ResponseWriter to capture the HTTP status code
// and Content-Type written by downstream handlers.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	contentType string
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.contentType = rw.Header().Get("Content-Type")
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.statusCode == 0 {
		rw.statusCode = http.StatusOK
		rw.contentType = rw.Header().Get("Content-Type")
	}
	return rw.ResponseWriter.Write(b)
}

// extractIP returns the client's IP address from the request.
// It prefers the leftmost entry in X-Forwarded-For (the original client),
// falling back to RemoteAddr.
func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	}
	// RemoteAddr is "host:port"; strip the port.
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host
}

// UnmarshalCaddyfile sets up the module from Caddyfile tokens.
//
// Syntax:
//
//	plausible {
//	    domain_name <domain>
//	    base_url    <url>                     # optional
//	    props       <prop1> [<prop2> ...]     # optional; content_type, device_type
//	}
func (m *PlausiblePlugin) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "domain_name":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.DomainName = d.Val()

			case "base_url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.BaseURL = d.Val()

			case "props":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				m.Props = args

			default:
				return d.Errf("unrecognized subdirective '%s'", d.Val())
			}
		}
	}
	return nil
}

// parseCaddyfile parses the Caddyfile directive.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m PlausiblePlugin
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*PlausiblePlugin)(nil)
	_ caddy.Validator             = (*PlausiblePlugin)(nil)
	_ caddyhttp.MiddlewareHandler = (*PlausiblePlugin)(nil)
	_ caddyfile.Unmarshaler       = (*PlausiblePlugin)(nil)
)
