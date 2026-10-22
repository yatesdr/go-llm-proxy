package httputil

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// Eidonix connect support.
//
// A backend URL of the form connect://<site>/<service>/<path> names a service
// at a remote site that the eidonix-connect hub makes reachable as plain HTTP
// at <hub>/<site>/<service>/<path>. The resolution happens in the shared
// transport (see connectRoundTripper), so any client from NewHTTPClient
// accepts such URLs; other code only has to avoid assuming http(s).

const (
	// DefaultConnectURL is the hub address when the config has no connect block.
	DefaultConnectURL = "http://connect:8080"
	// DefaultConnectApplication is the X-Eidonix-Application label.
	DefaultConnectApplication = "go-llm-proxy"

	// HeaderApplication and HeaderFor are the hub's optional request labels.
	HeaderApplication = "X-Eidonix-Application"
	HeaderFor         = "X-Eidonix-For"
	// HeaderError carries the hub's error code on its JSON error replies.
	HeaderError = "X-Eidonix-Error"

	connectScheme = "connect"
	maxLabelLen   = 64
)

// ConnectSettings is the resolved connect hub configuration.
type ConnectSettings struct {
	URL         string // hub base URL, no trailing slash
	Application string // sent as X-Eidonix-Application
}

var connectCfg atomic.Pointer[ConnectSettings]

func init() { SetConnect("", "") }

// SetConnect installs the hub URL and application label (empty = default).
// Safe to call at any time, e.g. on every config reload.
func SetConnect(hubURL, application string) {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		hubURL = DefaultConnectURL
	}
	application = SanitizeLabel(application)
	if application == "" {
		application = DefaultConnectApplication
	}
	connectCfg.Store(&ConnectSettings{URL: hubURL, Application: application})
}

// Connect returns the current hub settings.
func Connect() ConnectSettings { return *connectCfg.Load() }

// IsConnectURL reports whether raw is a connect:// URL.
func IsConnectURL(raw string) bool {
	return len(raw) >= len(connectScheme)+3 && strings.EqualFold(raw[:len(connectScheme)+3], connectScheme+"://")
}

// ParseConnectURL splits connect://<site>/<service>[/<path>] and validates it.
// path is "" or begins with "/".
func ParseConnectURL(raw string) (site, service, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid connect URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, connectScheme) {
		return "", "", "", fmt.Errorf("not a connect URL")
	}
	if u.User != nil {
		return "", "", "", fmt.Errorf("connect URL must not contain credentials")
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("connect URL needs a site: connect://<site>/<service>/<path>")
	}
	rest := strings.TrimPrefix(u.EscapedPath(), "/")
	service, tail, _ := strings.Cut(rest, "/")
	if service == "" {
		return "", "", "", fmt.Errorf("connect URL needs a service: connect://<site>/<service>/<path>")
	}
	if tail != "" {
		path = "/" + tail
	}
	return u.Host, service, path, nil
}

// ResolveConnectURL turns connect://<site>/<service>/<path>[?query] into the
// hub URL <hub>/<site>/<service>/<path>[?query] using the current settings.
func ResolveConnectURL(raw string) (string, error) {
	site, service, path, err := ParseConnectURL(raw)
	if err != nil {
		return "", err
	}
	out := Connect().URL + "/" + site + "/" + service + path
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		out += raw[i:]
	}
	return out, nil
}

// ResolveBackendURL returns the URL actually dialled for a configured backend
// URL: connect:// URLs are resolved to the hub, everything else is returned
// unchanged (as is a malformed connect URL, which fails at request time).
func ResolveBackendURL(raw string) string {
	if !IsConnectURL(raw) {
		return raw
	}
	if resolved, err := ResolveConnectURL(raw); err == nil {
		return resolved
	}
	return raw
}

// SanitizeLabel makes s safe for an X-Eidonix-* header: printable ASCII only,
// trimmed, at most 64 bytes.
func SanitizeLabel(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < maxLabelLen; i++ {
		if c := s[i]; c >= 0x20 && c <= 0x7e {
			b = append(b, c)
		}
	}
	return strings.TrimSpace(string(b))
}

type callerKey struct{}

type affinityKey struct{}

// HeaderAffinity carries a conversation's affinity key to a connect service,
// so a load balancer in front of several replicas there (haproxy: balance
// hdr(X-Affinity-Key)) keeps the conversation on the replica that holds its
// prefix cache. Backends listed directly are chosen here (lb.ResolveModel);
// a connect service's replicas are chosen at the site.
const HeaderAffinity = "X-Affinity-Key"

// WithAffinity records a request's conversation affinity key (lb.AffinityKey;
// 0 = none) for calls to connect backends.
func WithAffinity(ctx context.Context, key uint64) context.Context {
	return context.WithValue(ctx, affinityKey{}, key)
}

// AffinityFromContext returns the key stored by WithAffinity, or 0.
func AffinityFromContext(ctx context.Context) uint64 {
	k, _ := ctx.Value(affinityKey{}).(uint64)
	return k
}

// WithCaller records the name of the API key serving a request so that calls
// to connect backends can carry it as X-Eidonix-For.
func WithCaller(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, callerKey{}, name)
}

// CallerFromContext returns the name stored by WithCaller, or "".
func CallerFromContext(ctx context.Context) string {
	s, _ := ctx.Value(callerKey{}).(string)
	return s
}

// connectRoundTripper resolves connect:// requests to the hub and labels them.
// Requests with any other scheme pass through untouched.
type connectRoundTripper struct{ base http.RoundTripper }

func (c *connectRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, connectScheme) {
		return c.base.RoundTrip(req)
	}
	closeBody := func() {
		if req.Body != nil {
			req.Body.Close()
		}
	}
	resolved, err := ResolveConnectURL(req.URL.String())
	if err != nil {
		closeBody()
		return nil, err
	}
	target, err := url.Parse(resolved)
	if err != nil {
		closeBody()
		return nil, err
	}
	out := req.Clone(req.Context())
	out.URL = target
	out.Host = target.Host
	out.Header.Set(HeaderApplication, Connect().Application)
	if who := SanitizeLabel(CallerFromContext(req.Context())); who != "" {
		out.Header.Set(HeaderFor, who)
	} else {
		out.Header.Del(HeaderFor)
	}
	if k := AffinityFromContext(req.Context()); k != 0 {
		out.Header.Set(HeaderAffinity, fmt.Sprintf("%016x", k))
	} else {
		out.Header.Del(HeaderAffinity)
	}
	return c.base.RoundTrip(out)
}
