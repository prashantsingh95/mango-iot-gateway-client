package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// ---------- Edge protection (ngrok --basic-auth) ----------
//
// The public platform URL may sit behind edge HTTP Basic auth (e.g.
// `ngrok http --basic-auth user:pass`). Credentials travel in the config URL
// userinfo (`https://user:pass@host`), but the WS dialer and some HTTP paths
// reject userinfo URLs, so strip credentials here and send them as an
// Authorization header instead. Passwords are never logged; log only the
// stripped URL via stripEdgeAuth.
func splitEdgeAuth(rawURL string) (cleanURL string, header http.Header) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL, nil
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if user == "" {
		return rawURL, nil
	}
	u.User = nil
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return u.String(), http.Header{"Authorization": {"Basic " + token}}
}

// stripEdgeAuth returns the URL with any embedded credentials removed,
// safe for logs and error messages.
func stripEdgeAuth(rawURL string) string {
	clean, _ := splitEdgeAuth(rawURL)
	return clean
}

// edgeAuthHeader returns just the Authorization header for a base URL,
// or nil when the URL carries no credentials.
func edgeAuthHeader(baseURL string) http.Header {
	_, h := splitEdgeAuth(baseURL)
	return h
}

// applyEdgeAuth attaches edge protection credentials to an outbound platform
// request: the configured gateway.api_key as X-API-Key (primary, JWT-style),
// plus HTTP Basic derived from URL userinfo when present (ngrok --basic-auth
// fallback). Secrets never leave via logs — callers must log stripped URLs.
func applyEdgeAuth(req *http.Request, baseURL string) {
	if k := strings.TrimSpace(cfg.Gateway.APIKey); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	if _, h := splitEdgeAuth(baseURL); h != nil {
		for k, vs := range h {
			req.Header[k] = vs
		}
	}
}
