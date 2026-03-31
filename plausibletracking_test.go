package caddy_plausible_plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// newTestPlugin returns a PlausiblePlugin wired up with a nop logger and an
// HTTP client that talks to targetURL (typically an httptest.Server URL).
func newTestPlugin(targetURL string) *PlausiblePlugin {
	return &PlausiblePlugin{
		DomainName: "example.com",
		BaseURL:    targetURL,
		logger:     zap.NewNop(),
		client:     &http.Client{Timeout: 2 * time.Second},
	}
}

// waitForEvent blocks until the channel receives a value or the test times out.
func waitForEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for plausible event to be received")
	}
}

// ── extractIP ────────────────────────────────────────────────────────────────

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "XFF single IP",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.5",
			want:       "203.0.113.5",
		},
		{
			name:       "XFF multiple IPs takes first",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.5, 10.0.0.2, 10.0.0.3",
			want:       "203.0.113.5",
		},
		{
			name:       "XFF with leading space",
			remoteAddr: "10.0.0.1:1234",
			xff:        " 203.0.113.5 , 10.0.0.2",
			want:       "203.0.113.5",
		},
		{
			name:       "no XFF falls back to RemoteAddr",
			remoteAddr: "203.0.113.9:5678",
			xff:        "",
			want:       "203.0.113.9",
		},
		{
			name:       "RemoteAddr without port",
			remoteAddr: "203.0.113.9",
			xff:        "",
			want:       "203.0.113.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			got := extractIP(r)
			if got != tt.want {
				t.Errorf("extractIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── responseWriter ───────────────────────────────────────────────────────────

func TestResponseWriter_CapturesStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	rw.WriteHeader(http.StatusCreated)

	if rw.statusCode != http.StatusCreated {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusCreated)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("underlying recorder code = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestResponseWriter_CapturesContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusOK)

	if rw.contentType != "text/html; charset=utf-8" {
		t.Errorf("contentType = %q, want %q", rw.contentType, "text/html; charset=utf-8")
	}
}

func TestResponseWriter_WriteDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	_, _ = rw.Write([]byte("hello"))

	if rw.statusCode != http.StatusOK {
		t.Errorf("statusCode after Write = %d, want 200", rw.statusCode)
	}
}

func TestResponseWriter_WriteAfterWriteHeaderKeepsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	rw.WriteHeader(http.StatusAccepted)
	_, _ = rw.Write([]byte("body"))

	if rw.statusCode != http.StatusAccepted {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusAccepted)
	}
}

// ── static asset filter ──────────────────────────────────────────────────────

func TestStaticAssetFilter(t *testing.T) {
	tests := []struct {
		path    string
		skipped bool
	}{
		{"/style.css", true},
		{"/app.js", true},
		{"/logo.png", true},
		{"/photo.jpg", true},
		{"/photo.jpeg", true},
		{"/anim.gif", true},
		{"/icon.svg", true},
		{"/image.webp", true},
		{"/favicon.ico", true},
		{"/font.woff", true},
		{"/font.woff2", true},
		{"/font.ttf", true},
		{"/bundle.js.map", true},
		{"/video.mp4", true},
		{"/", false},
		{"/about", false},
		{"/blog/post", false},
		{"/index.html", false},
		{"/page.php", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := regexStaticAssets.MatchString(tt.path)
			if got != tt.skipped {
				t.Errorf("regexStaticAssets.MatchString(%q) = %v, want %v", tt.path, got, tt.skipped)
			}
		})
	}
}

// ── detectDeviceType ─────────────────────────────────────────────────────────

func TestDetectDeviceType(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want string
	}{
		{
			name: "Googlebot",
			ua:   "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			want: "bot",
		},
		{
			name: "Bingbot",
			ua:   "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
			want: "bot",
		},
		{
			name: "generic crawler",
			ua:   "MyScraper/1.0 (crawler)",
			want: "bot",
		},
		{
			name: "iPad",
			ua:   "Mozilla/5.0 (iPad; CPU OS 16_0 like Mac OS X) AppleWebKit/605.1.15",
			want: "tablet",
		},
		{
			name: "Android tablet",
			ua:   "Mozilla/5.0 (Linux; Android 13; SM-T870) AppleWebKit/537.36 tablet",
			want: "tablet",
		},
		{
			name: "iPhone",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15",
			want: "mobile",
		},
		{
			name: "Android phone",
			ua:   "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 Mobile Safari/537.36",
			want: "mobile",
		},
		{
			name: "Windows desktop Chrome",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
			want: "desktop",
		},
		{
			name: "macOS Safari",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Version/17.0 Safari/605.1.15",
			want: "desktop",
		},
		{
			name: "empty user agent",
			ua:   "",
			want: "desktop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectDeviceType(tt.ua)
			if got != tt.want {
				t.Errorf("detectDeviceType() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── buildProps ───────────────────────────────────────────────────────────────

func TestBuildProps_NoneConfigured(t *testing.T) {
	plugin := newTestPlugin("http://unused")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	props := plugin.buildProps(r, "text/html")
	if props != nil {
		t.Errorf("expected nil props when none configured, got %v", props)
	}
}

func TestBuildProps_ContentType(t *testing.T) {
	plugin := newTestPlugin("http://unused")
	plugin.Props = []string{PropContentType}

	tests := []struct {
		ct   string
		want string
	}{
		{"text/html; charset=utf-8", "text/html"},
		{"application/json", "application/json"},
		{"application/json; charset=utf-8", "application/json"},
		{"", ""},
	}

	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		props := plugin.buildProps(r, tt.ct)
		if tt.want == "" {
			if props != nil {
				t.Errorf("content_type=%q: expected nil props, got %v", tt.ct, props)
			}
			continue
		}
		if props == nil {
			t.Fatalf("content_type=%q: expected props, got nil", tt.ct)
		}
		if got := props[PropContentType]; got != tt.want {
			t.Errorf("content_type=%q: props[content_type] = %q, want %q", tt.ct, got, tt.want)
		}
	}
}

func TestBuildProps_DeviceType(t *testing.T) {
	plugin := newTestPlugin("http://unused")
	plugin.Props = []string{PropDeviceType}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)")

	props := plugin.buildProps(r, "")
	if props == nil {
		t.Fatal("expected props, got nil")
	}
	if got := props[PropDeviceType]; got != "mobile" {
		t.Errorf("props[device_type] = %q, want mobile", got)
	}
}

func TestBuildProps_Both(t *testing.T) {
	plugin := newTestPlugin("http://unused")
	plugin.Props = []string{PropContentType, PropDeviceType}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120")

	props := plugin.buildProps(r, "text/html; charset=utf-8")
	if props == nil {
		t.Fatal("expected props, got nil")
	}
	if got := props[PropContentType]; got != "text/html" {
		t.Errorf("props[content_type] = %q, want text/html", got)
	}
	if got := props[PropDeviceType]; got != "desktop" {
		t.Errorf("props[device_type] = %q, want desktop", got)
	}
}

// ── recordEvent filtering ────────────────────────────────────────────────────

func TestRecordEvent_SkipsErrorStatuses(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	plugin := newTestPlugin(server.URL)

	for _, status := range []int{400, 404, 500, 503} {
		r := httptest.NewRequest(http.MethodGet, "/page", nil)
		plugin.recordEvent(r, status, "")
	}

	select {
	case <-received:
		t.Error("expected no event to be sent for error status codes")
	case <-time.After(100 * time.Millisecond):
		// correct: nothing received
	}
}

func TestRecordEvent_SkipsStaticAssets(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	plugin := newTestPlugin(server.URL)

	for _, path := range []string{"/style.css", "/app.js", "/logo.png", "/font.woff2"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		plugin.recordEvent(r, http.StatusOK, "")
	}

	select {
	case <-received:
		t.Error("expected no event to be sent for static assets")
	case <-time.After(100 * time.Millisecond):
		// correct
	}
}

// ── recordEvent payload ──────────────────────────────────────────────────────

type capturedEvent struct {
	path      string
	userAgent string
	xForward  string
	payload   EventPayload
}

func TestRecordEvent_SendsCorrectPayload(t *testing.T) {
	received := make(chan capturedEvent, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var p EventPayload
		_ = json.Unmarshal(body, &p)
		received <- capturedEvent{
			path:      r.URL.Path,
			userAgent: r.Header.Get("User-Agent"),
			xForward:  r.Header.Get("X-Forwarded-For"),
			payload:   p,
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	plugin := newTestPlugin(server.URL)

	r := httptest.NewRequest(http.MethodGet, "/blog/post?utm_source=test", nil)
	r.Header.Set("User-Agent", "TestBrowser/1.0")
	r.Header.Set("Referer", "https://referrer.example.com/")
	r.RemoteAddr = "203.0.113.42:9000"

	plugin.recordEvent(r, http.StatusOK, "")

	select {
	case ev := <-received:
		if ev.path != "/api/event" {
			t.Errorf("path = %q, want /api/event", ev.path)
		}
		if ev.userAgent != "TestBrowser/1.0" {
			t.Errorf("User-Agent = %q, want TestBrowser/1.0", ev.userAgent)
		}
		if ev.xForward != "203.0.113.42" {
			t.Errorf("X-Forwarded-For = %q, want 203.0.113.42", ev.xForward)
		}
		if ev.payload.Name != "pageview" {
			t.Errorf("payload.Name = %q, want pageview", ev.payload.Name)
		}
		if ev.payload.Domain != "example.com" {
			t.Errorf("payload.Domain = %q, want example.com", ev.payload.Domain)
		}
		if ev.payload.Url != "/blog/post?utm_source=test" {
			t.Errorf("payload.Url = %q, want /blog/post?utm_source=test", ev.payload.Url)
		}
		if ev.payload.Referrer != "https://referrer.example.com/" {
			t.Errorf("payload.Referrer = %q, want https://referrer.example.com/", ev.payload.Referrer)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestRecordEvent_SendsCustomProps(t *testing.T) {
	received := make(chan EventPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p EventPayload
		_ = json.Unmarshal(body, &p)
		received <- p
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	plugin := newTestPlugin(server.URL)
	plugin.Props = []string{PropContentType, PropDeviceType}

	r := httptest.NewRequest(http.MethodGet, "/page", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)")

	plugin.recordEvent(r, http.StatusOK, "text/html; charset=utf-8")

	select {
	case p := <-received:
		if p.Props == nil {
			t.Fatal("expected props in payload, got nil")
		}
		if got := p.Props[PropContentType]; got != "text/html" {
			t.Errorf("props[content_type] = %q, want text/html", got)
		}
		if got := p.Props[PropDeviceType]; got != "mobile" {
			t.Errorf("props[device_type] = %q, want mobile", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// ── ServeHTTP integration ────────────────────────────────────────────────────

func TestServeHTTP_RecordsPageviewAfterResponse(t *testing.T) {
	eventReceived := make(chan struct{}, 1)
	plausibleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(eventReceived)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer plausibleServer.Close()

	plugin := newTestPlugin(plausibleServer.URL)

	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("page content"))
		return nil
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/about", nil)

	if err := plugin.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("response status = %d, want 200", rec.Code)
	}

	waitForEvent(t, eventReceived)
}

func TestServeHTTP_ForwardsContentTypeToProps(t *testing.T) {
	received := make(chan EventPayload, 1)
	plausibleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p EventPayload
		_ = json.Unmarshal(body, &p)
		received <- p
		w.WriteHeader(http.StatusAccepted)
	}))
	defer plausibleServer.Close()

	plugin := newTestPlugin(plausibleServer.URL)
	plugin.Props = []string{PropContentType}

	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
		return nil
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/data", nil)

	if err := plugin.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	select {
	case p := <-received:
		if p.Props == nil {
			t.Fatal("expected props, got nil")
		}
		if got := p.Props[PropContentType]; got != "application/json" {
			t.Errorf("props[content_type] = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestServeHTTP_DoesNotRecordForStaticAsset(t *testing.T) {
	eventReceived := make(chan struct{}, 1)
	plausibleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventReceived <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer plausibleServer.Close()

	plugin := newTestPlugin(plausibleServer.URL)

	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/style.css", nil)

	if err := plugin.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	select {
	case <-eventReceived:
		t.Error("expected no event for static asset")
	case <-time.After(100 * time.Millisecond):
		// correct
	}
}

func TestServeHTTP_PassesThroughNextError(t *testing.T) {
	plugin := newTestPlugin("http://unused.example.com")

	wantErr := caddyhttp.Error(http.StatusInternalServerError, nil)
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return wantErr
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/page", nil)

	err := plugin.ServeHTTP(rec, r, next)
	if err == nil {
		t.Fatal("expected error from ServeHTTP, got nil")
	}
}

// ── UnmarshalCaddyfile ───────────────────────────────────────────────────────

func TestUnmarshalCaddyfile_ParsesDomainAndBaseURL(t *testing.T) {
	input := `plausible {
		domain_name example.com
		base_url    https://custom.plausible.io
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.DomainName != "example.com" {
		t.Errorf("DomainName = %q, want example.com", m.DomainName)
	}
	if m.BaseURL != "https://custom.plausible.io" {
		t.Errorf("BaseURL = %q, want https://custom.plausible.io", m.BaseURL)
	}
}

func TestUnmarshalCaddyfile_ParsesProps(t *testing.T) {
	input := `plausible {
		domain_name example.com
		props       content_type device_type
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.Props) != 2 || m.Props[0] != PropContentType || m.Props[1] != PropDeviceType {
		t.Errorf("Props = %v, want [content_type device_type]", m.Props)
	}
}

func TestUnmarshalCaddyfile_DomainNameOnly(t *testing.T) {
	input := `plausible {
		domain_name mysite.io
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.DomainName != "mysite.io" {
		t.Errorf("DomainName = %q, want mysite.io", m.DomainName)
	}
	if m.BaseURL != "" {
		t.Errorf("BaseURL should be empty when not provided, got %q", m.BaseURL)
	}
}

func TestUnmarshalCaddyfile_UnknownDirectiveReturnsError(t *testing.T) {
	input := `plausible {
		domain_name example.com
		unknown_key value
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Error("expected error for unknown directive, got nil")
	}
}

func TestUnmarshalCaddyfile_MissingArgReturnsError(t *testing.T) {
	input := `plausible {
		domain_name
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Error("expected error when domain_name has no argument, got nil")
	}
}

func TestUnmarshalCaddyfile_MissingPropsArgReturnsError(t *testing.T) {
	input := `plausible {
		domain_name example.com
		props
	}`

	d := caddyfile.NewTestDispenser(input)
	var m PlausiblePlugin
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Error("expected error when props has no arguments, got nil")
	}
}
