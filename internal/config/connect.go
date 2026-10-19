package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-llm-proxy/internal/httputil"
)

// The Eidonix connect hub publishes what each remote site offers at
// GET /v1/catalog and GET /v1/catalog/<site>. Only the fields the proxy uses
// are decoded here; unknown fields are ignored so the hub can grow.

// HubCatalog is the hub's catalog reply.
type HubCatalog struct {
	Sites []HubSite `json:"sites"`
}

// HubSite is one remote site.
type HubSite struct {
	Name     string       `json:"name"`
	Online   bool         `json:"online"`
	Services []HubService `json:"services"`
}

// HubService is one service a site offers.
type HubService struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models,omitempty"`
}

// FetchHubCatalog reads the catalog from the hub: the whole of it when site is
// empty, otherwise one site's. A non-200 reply becomes an error carrying the
// hub's own message.
func FetchHubCatalog(ctx context.Context, client *http.Client, hubURL, application, site string) (*HubCatalog, error) {
	target := strings.TrimRight(hubURL, "/") + "/v1/catalog"
	if site != "" {
		target += "/" + url.PathEscape(site)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(httputil.HeaderApplication, application)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connect hub: %s", hubErrorText(resp, body))
	}
	var cat HubCatalog
	if err := json.Unmarshal(body, &cat); err != nil {
		return nil, fmt.Errorf("connect hub: unreadable catalog: %w", err)
	}
	return &cat, nil
}

// hubErrorText renders a hub error reply: its error code and message when it
// sent the documented JSON shape, otherwise the HTTP status.
func hubErrorText(resp *http.Response, body []byte) string {
	code := resp.Header.Get(httputil.HeaderError)
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		if code == "" {
			code = e.Error.Code
		}
		if e.Error.Message != "" {
			if code != "" {
				return code + ": " + e.Error.Message
			}
			return e.Error.Message
		}
	}
	if code != "" {
		return fmt.Sprintf("%s (HTTP %d)", code, resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// serviceState judges one connect backend against a site catalog: up, or down
// with the hub's reason.
func (c *HubCatalog) serviceState(site, service string) (up bool, reason string) {
	for _, s := range c.Sites {
		if s.Name != site {
			continue
		}
		if !s.Online {
			return false, fmt.Sprintf("site %s is offline", site)
		}
		for _, svc := range s.Services {
			if svc.Name != service {
				continue
			}
			if !svc.Healthy {
				return false, fmt.Sprintf("service %s at site %s is reported unhealthy", service, site)
			}
			return true, ""
		}
		return false, fmt.Sprintf("site %s does not offer service %s", site, service)
	}
	return false, fmt.Sprintf("unknown site %s", site)
}

// connectPollInterval is how often the hub catalog is polled for connect
// backends: one request per site.
var connectPollInterval = 15 * time.Second

// CheckConnectBackend asks the hub whether the service behind a connect://
// URL is usable right now: site online, service offered and healthy. Used by
// the admin "Test" buttons, which must not probe through the hub.
func CheckConnectBackend(ctx context.Context, client *http.Client, rawURL string) (up bool, reason string) {
	site, service, _, err := httputil.ParseConnectURL(rawURL)
	if err != nil {
		return false, err.Error()
	}
	hub := httputil.Connect()
	cat, err := FetchHubCatalog(ctx, client, hub.URL, hub.Application, site)
	if err != nil {
		return false, err.Error()
	}
	return cat.serviceState(site, service)
}
