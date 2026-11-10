package core

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Generic OpenID Connect providers (Keycloak, Auth0, Okta, Azure AD / Entra,
// Authentik, Zitadel, GitLab, ...). Configured per project with an issuer
// URL, client ID and secret; endpoints come from the issuer's discovery
// document. Works with supabase-js unchanged:
//   supabase.auth.signInWithOAuth({ provider: 'keycloak' })        // browser redirect
//   supabase.auth.signInWithIdToken({ provider: 'keycloak', token }) // native apps
// id_tokens must be RS256-signed (the OIDC default).

type OIDCProvider struct {
	Name         string `json:"name"`  // what apps pass as `provider`, e.g. "keycloak"
	Label        string `json:"label"` // shown in the UI
	Enabled      bool   `json:"enabled"`
	Issuer       string `json:"issuer"` // e.g. https://sso.example.com/realms/main
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Scopes       string `json:"scopes"` // default "openid email profile"
}

var oidcName = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,30}$`)

var reservedProviders = map[string]bool{"email": true, "phone": true, "anonymous": true, "google": true, "github": true, "apple": true}

func (c AuthConfig) oidc(name string) (OIDCProvider, bool) {
	for _, p := range c.OIDC {
		if p.Name == name {
			return p, p.Enabled && p.Issuer != "" && p.ClientID != ""
		}
	}
	return OIDCProvider{}, false
}

// externalProviders is the `external` map of GET /settings.
func (c AuthConfig) externalProviders() map[string]any {
	m := map[string]any{"email": true, "phone": c.Phone.Enabled, "google": c.Google.Enabled, "github": c.GitHub.Enabled,
		"apple": c.Apple.Enabled, "anonymous_users": c.AnonymousUsers}
	for _, p := range c.OIDC {
		m[p.Name] = p.Enabled
	}
	return m
}

func validateOIDC(list []OIDCProvider) error {
	seen := map[string]bool{}
	for _, p := range list {
		if !oidcName.MatchString(p.Name) {
			return errBad("invalid_parameter", fmt.Sprintf("OIDC provider name %q: use 2-31 lowercase letters, digits, - or _", p.Name), "")
		}
		if reservedProviders[p.Name] {
			return errBad("invalid_parameter", fmt.Sprintf("%q is a built-in provider; pick another name", p.Name), "")
		}
		if seen[p.Name] {
			return errBad("invalid_parameter", "duplicate OIDC provider "+p.Name, "")
		}
		seen[p.Name] = true
		if p.Enabled {
			u, err := url.Parse(p.Issuer)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errBad("invalid_parameter", fmt.Sprintf("OIDC provider %s: issuer must be an https URL", p.Name), "")
			}
			if p.ClientID == "" || p.ClientSecret == "" {
				return errBad("invalid_parameter", fmt.Sprintf("OIDC provider %s needs a client ID and secret before it can be enabled", p.Name), "")
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- discovery + keys

type oidcMeta struct {
	Issuer   string `json:"issuer"`
	AuthURL  string `json:"authorization_endpoint"`
	TokenURL string `json:"token_endpoint"`
	UserInfo string `json:"userinfo_endpoint"`
	JWKSURI  string `json:"jwks_uri"`

	keys    map[string]*rsa.PublicKey
	keysAt  time.Time
	fetched time.Time
}

var (
	oidcMu    sync.Mutex
	oidcCache = map[string]*oidcMeta{}
)

func oidcDiscover(ctx context.Context, issuer string) (*oidcMeta, error) {
	issuer = strings.TrimRight(issuer, "/")
	oidcMu.Lock()
	m := oidcCache[issuer]
	oidcMu.Unlock()
	if m != nil && time.Since(m.fetched) < time.Hour {
		return m, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", issuer+"/.well-known/openid-configuration", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OIDC discovery: %s returned %s", issuer, resp.Status)
	}
	var fresh oidcMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&fresh); err != nil {
		return nil, fmt.Errorf("OIDC discovery: %v", err)
	}
	if strings.TrimRight(fresh.Issuer, "/") != issuer {
		return nil, fmt.Errorf("OIDC discovery: issuer is %q, expected %q", fresh.Issuer, issuer)
	}
	if fresh.AuthURL == "" || fresh.TokenURL == "" || fresh.JWKSURI == "" {
		return nil, fmt.Errorf("OIDC discovery: document is missing endpoints")
	}
	fresh.fetched = time.Now()
	oidcMu.Lock()
	if m != nil { // keep keys across a discovery refresh
		fresh.keys, fresh.keysAt = m.keys, m.keysAt
	}
	oidcCache[issuer] = &fresh
	oidcMu.Unlock()
	return &fresh, nil
}

func (m *oidcMeta) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	oidcMu.Lock()
	k, keysAt := m.keys[kid], m.keysAt
	oidcMu.Unlock()
	if k != nil && time.Since(keysAt) < time.Hour {
		return k, nil
	}
	if time.Since(keysAt) > time.Minute || m.keys == nil {
		keys, err := fetchJWKS(ctx, m.JWKSURI)
		if err != nil && m.keys == nil {
			return nil, err
		}
		if err == nil {
			oidcMu.Lock()
			m.keys, m.keysAt = keys, time.Now()
			oidcMu.Unlock()
		}
	}
	oidcMu.Lock()
	defer oidcMu.Unlock()
	if k := m.keys[kid]; k != nil {
		return k, nil
	}
	if kid == "" && len(m.keys) == 1 {
		for _, k := range m.keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func verifyOIDCToken(ctx context.Context, p OIDCProvider, token string) (map[string]any, error) {
	m, err := oidcDiscover(ctx, p.Issuer)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed id_token")
	}
	var hdr struct{ Alg, Kid string }
	hb, err := b64url.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "RS256" {
		return nil, fmt.Errorf("id_token must be signed with RS256")
	}
	key, err := m.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := b64url.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed id_token signature")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, h[:], sig) != nil {
		return nil, fmt.Errorf("id_token signature is invalid")
	}
	cb, err := b64url.DecodeString(parts[1])
	var c map[string]any
	if err != nil || json.Unmarshal(cb, &c) != nil {
		return nil, fmt.Errorf("malformed id_token claims")
	}
	if strings.TrimRight(str(c, "iss"), "/") != strings.TrimRight(p.Issuer, "/") {
		return nil, fmt.Errorf("id_token issuer %q is not %s", str(c, "iss"), p.Issuer)
	}
	if !audMatches(c["aud"], []string{p.ClientID}) {
		return nil, fmt.Errorf("id_token audience %v is not this project's client ID", c["aud"])
	}
	now := float64(time.Now().Unix())
	if exp, _ := c["exp"].(float64); exp == 0 || now > exp+60 {
		return nil, fmt.Errorf("id_token has expired")
	}
	if iat, _ := c["iat"].(float64); iat > now+300 {
		return nil, fmt.Errorf("id_token was issued in the future")
	}
	return c, nil
}

func oidcProfile(c map[string]any) *oauthProfileData {
	name := str(c, "name")
	if name == "" {
		name = strings.TrimSpace(str(c, "given_name") + " " + str(c, "family_name"))
	}
	if name == "" {
		name = str(c, "preferred_username")
	}
	return &oauthProfileData{ID: str(c, "sub"), Email: strings.ToLower(str(c, "email")), Verified: truthy(c["email_verified"]),
		Name: name, Avatar: str(c, "picture"), Raw: c}
}

// ---------------------------------------------------------------- browser flow

func (x *authReq) oidcAuthorize(ctx context.Context, p OIDCProvider) {
	m, err := oidcDiscover(ctx, p.Issuer)
	if err != nil {
		writeAuthError(x.w, authError(502, "provider_unreachable", err.Error()))
		return
	}
	q := x.r.URL.Query()
	redirect := q.Get("redirect_to")
	if redirect != "" && !x.cfg.redirectAllowed(redirect) {
		redirect = ""
	}
	nonce := randHex(16)
	state := signJWT(x.secret, map[string]any{
		"typ": "oauth_state", "p": p.Name, "r": redirect, "no": nonce,
		"cc":  pkceChallenge(q.Get("code_challenge"), q.Get("code_challenge_method")),
		"exp": time.Now().Add(10 * time.Minute).Unix(), "n": randHex(8),
	})
	scope := p.Scopes
	if scope == "" {
		scope = "openid email profile"
	}
	if !strings.Contains(" "+scope+" ", " openid ") {
		scope = "openid " + scope
	}
	if extra := q.Get("scopes"); extra != "" {
		scope += " " + extra
	}
	v := url.Values{"client_id": {p.ClientID}, "redirect_uri": {x.callbackURL()}, "response_type": {"code"}, "scope": {scope}, "state": {state}, "nonce": {nonce}}
	sep := "?"
	if strings.Contains(m.AuthURL, "?") {
		sep = "&"
	}
	http.Redirect(x.w, x.r, m.AuthURL+sep+v.Encode(), http.StatusFound)
}

func (x *authReq) oidcCallback(ctx context.Context, p OIDCProvider, claims map[string]any, code string, fail func(code, msg string)) (*authUser, string, bool) {
	m, err := oidcDiscover(ctx, p.Issuer)
	if err != nil {
		fail("provider_unreachable", err.Error())
		return nil, "", false
	}
	form := url.Values{"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "code": {code},
		"redirect_uri": {x.callbackURL()}, "grant_type": {"authorization_code"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", m.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fail("oauth_exchange_failed", err.Error())
		return nil, "", false
	}
	defer resp.Body.Close()
	var tr oauthTokens
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr)
	if tr.IDToken == "" {
		fail("oauth_exchange_failed", fmt.Sprintf("token exchange failed: %s %s", tr.Error, tr.ErrorDesc))
		return nil, "", false
	}
	c, err := verifyOIDCToken(ctx, p, tr.IDToken)
	if err != nil {
		fail("oauth_profile_failed", err.Error())
		return nil, "", false
	}
	if want, _ := claims["no"].(string); want == "" || str(c, "nonce") != want {
		fail("oauth_profile_failed", "id_token nonce does not match")
		return nil, "", false
	}
	prof := oidcProfile(c)
	if prof.Email == "" && m.UserInfo != "" && tr.AccessToken != "" {
		info := map[string]any{}
		if getJSON(ctx, m.UserInfo, tr.AccessToken, &info) == nil && str(info, "sub") == prof.ID {
			if e := strings.ToLower(str(info, "email")); e != "" {
				prof.Email, prof.Verified = e, truthy(info["email_verified"])
			}
		}
	}
	u, err := x.oauthUser(ctx, p.Name, prof)
	if err != nil {
		ae := AsAPIError(err)
		fail(ae.Code, ae.Message)
		return nil, "", false
	}
	return u, tr.AccessToken, true
}

// oidcIDTokenGrant: signInWithIdToken({ provider: '<name>', token, nonce? }).
func (x *authReq) oidcIDTokenGrant(ctx context.Context, p OIDCProvider, body map[string]any) (any, error) {
	tok := str(body, "id_token")
	if tok == "" {
		return nil, authError(400, "validation_failed", "id_token required")
	}
	c, err := verifyOIDCToken(ctx, p, tok)
	if err != nil {
		return nil, authError(400, "bad_oauth_callback", "Bad ID token: "+err.Error())
	}
	if err := checkNonce(str(body, "nonce"), c); err != nil {
		return nil, authError(400, "bad_oauth_callback", "Bad ID token: "+err.Error())
	}
	u, err := x.oauthUser(ctx, p.Name, oidcProfile(c))
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "login", u.ID, u.Email.String, map[string]any{"provider": p.Name, "grant": "id_token"})
	return x.issueSession(ctx, u, "oauth", x.ua, x.ip)
}
