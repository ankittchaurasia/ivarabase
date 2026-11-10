package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func bcryptCompare(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// OAuth sign-in (Google, GitHub, Apple - see auth_apple.go). The browser goes:
//   /auth/v1/authorize?provider=google&redirect_to=...  ->  provider consent
//   -> /auth/v1/callback?code&state  ->  redirect_to (#tokens or ?code= for PKCE)
// The state parameter is a short-lived JWT signed with the project secret.

type oauthEndpoints struct {
	auth, token, scope string
}

var oauthProviders = map[string]oauthEndpoints{
	"google": {"https://accounts.google.com/o/oauth2/v2/auth", "https://oauth2.googleapis.com/token", "openid email profile"},
	"github": {"https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token", "read:user user:email"},
}

func (c AuthConfig) provider(name string) (OAuthProvider, bool) {
	switch name {
	case "google":
		return c.Google, c.Google.Enabled && c.Google.ClientID != ""
	case "github":
		return c.GitHub, c.GitHub.Enabled && c.GitHub.ClientID != ""
	case "apple": // the web flow; native apps use signInWithIdToken
		return OAuthProvider{Enabled: c.Apple.Enabled, ClientID: c.Apple.ClientID}, c.Apple.Enabled && c.Apple.webReady()
	}
	return OAuthProvider{}, false
}

func (s *authSvc) callbackURL() string { return s.app.projectURL(s.slug) + "/auth/v1/callback" }

func (x *authReq) authorize(ctx context.Context) {
	q := x.r.URL.Query()
	name := q.Get("provider")
	if o, ok := x.cfg.oidc(name); ok {
		x.oidcAuthorize(ctx, o)
		return
	}
	p, ok := x.cfg.provider(name)
	ep, known := oauthProviders[name]
	if !ok || !known {
		writeAuthError(x.w, authError(400, "validation_failed", "Unsupported provider: provider is not enabled"))
		return
	}
	redirect := q.Get("redirect_to")
	if redirect != "" && !x.cfg.redirectAllowed(redirect) {
		redirect = ""
	}
	state := signJWT(x.secret, map[string]any{
		"typ": "oauth_state", "p": name, "r": redirect,
		"cc":  pkceChallenge(q.Get("code_challenge"), q.Get("code_challenge_method")),
		"exp": time.Now().Add(10 * time.Minute).Unix(), "n": randHex(8),
	})
	scope := ep.scope
	if extra := q.Get("scopes"); extra != "" {
		scope += " " + extra
	}
	v := url.Values{"client_id": {p.ClientID}, "redirect_uri": {x.callbackURL()}, "response_type": {"code"}, "scope": {scope}, "state": {state}}
	switch name {
	case "google":
		v.Set("access_type", "offline")
		v.Set("prompt", "select_account")
	case "apple":
		v.Set("response_mode", "form_post") // required by Apple when asking for name/email
	}
	http.Redirect(x.w, x.r, ep.auth+"?"+v.Encode(), http.StatusFound)
}

func (x *authReq) callback(ctx context.Context) {
	// Apple POSTs the result as a form; the others redirect with a query string.
	x.r.Body = http.MaxBytesReader(x.w, x.r.Body, 64<<10)
	x.r.ParseForm()
	q := x.r.Form
	claims, err := verifyJWT(x.secret, q.Get("state"))
	site := x.cfg.SiteURL
	if err != nil || claims["typ"] != "oauth_state" {
		x.redirectFragment(site, url.Values{"error": {"invalid_request"}, "error_code": {"bad_oauth_state"}, "error_description": {"OAuth state is invalid or expired"}})
		return
	}
	name, _ := claims["p"].(string)
	redirect, _ := claims["r"].(string)
	challenge, _ := claims["cc"].(string)
	redirect = x.cfg.redirectOrSite(redirect)
	fail := func(code, msg string) {
		x.redirectFragment(redirect, url.Values{"error": {"access_denied"}, "error_code": {code}, "error_description": {msg}})
	}
	if e := q.Get("error"); e != "" {
		fail("provider_error", q.Get("error_description")+" ("+e+")")
		return
	}
	if o, ok := x.cfg.oidc(name); ok {
		u, token, ok := x.oidcCallback(ctx, o, claims, q.Get("code"), fail)
		if !ok {
			return
		}
		authAudit(ctx, x.db, x.ip, "login", u.ID, u.Email.String, map[string]any{"provider": name})
		x.finishBrowserLogin(ctx, u, "signup", "oauth", challenge, token, redirect)
		return
	}
	p, ok := x.cfg.provider(name)
	if !ok {
		fail("provider_disabled", "Provider is not enabled")
		return
	}
	if name == "apple" {
		if p.ClientSecret, err = appleClientSecret(x.cfg.Apple); err != nil {
			fail("provider_misconfigured", err.Error())
			return
		}
	}
	tr, err := oauthExchange(ctx, name, p, q.Get("code"), x.callbackURL())
	if err != nil {
		fail("oauth_exchange_failed", err.Error())
		return
	}
	token := tr.AccessToken
	var prof *oauthProfileData
	if name == "apple" {
		// the profile is in Apple's id_token; the name only comes once, in the form
		claims, verr := verifyIDToken(ctx, "apple", tr.IDToken, []string{x.cfg.Apple.ClientID})
		if verr != nil {
			fail("oauth_profile_failed", verr.Error())
			return
		}
		prof = idTokenProfile("apple", claims)
		prof.Name = appleUserName(q.Get("user"))
	} else if prof, err = oauthProfile(ctx, name, token); err != nil {
		fail("oauth_profile_failed", err.Error())
		return
	}
	u, err := x.oauthUser(ctx, name, prof)
	if err != nil {
		ae := AsAPIError(err)
		fail(ae.Code, ae.Message)
		return
	}
	authAudit(ctx, x.db, x.ip, "login", u.ID, u.Email.String, map[string]any{"provider": name})
	x.finishBrowserLogin(ctx, u, "signup", "oauth", challenge, token, redirect)
}

type oauthProfileData struct {
	ID       string
	Email    string
	Verified bool
	Name     string
	Avatar   string
	Raw      map[string]any
}

type oauthTokens struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

func oauthExchange(ctx context.Context, name string, p OAuthProvider, code, redirectURI string) (*oauthTokens, error) {
	form := url.Values{"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "code": {code},
		"redirect_uri": {redirectURI}, "grant_type": {"authorization_code"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", oauthProviders[name].token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oauthTokens
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if out.AccessToken == "" && out.IDToken == "" {
		return nil, fmt.Errorf("token exchange failed: %s %s", out.Error, out.ErrorDesc)
	}
	if name == "apple" && out.IDToken == "" {
		return nil, fmt.Errorf("Apple returned no id_token")
	}
	return &out, nil
}

func getJSON(ctx context.Context, u, token string, dst any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

func oauthProfile(ctx context.Context, name, token string) (*oauthProfileData, error) {
	raw := map[string]any{}
	switch name {
	case "google":
		if err := getJSON(ctx, "https://openidconnect.googleapis.com/v1/userinfo", token, &raw); err != nil {
			return nil, err
		}
		v, _ := raw["email_verified"].(bool)
		return &oauthProfileData{ID: str(raw, "sub"), Email: strings.ToLower(str(raw, "email")), Verified: v, Name: str(raw, "name"), Avatar: str(raw, "picture"), Raw: raw}, nil
	case "github":
		if err := getJSON(ctx, "https://api.github.com/user", token, &raw); err != nil {
			return nil, err
		}
		p := &oauthProfileData{ID: fmt.Sprint(raw["id"]), Name: str(raw, "name"), Avatar: str(raw, "avatar_url"), Raw: raw}
		if p.Name == "" {
			p.Name = str(raw, "login")
		}
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if getJSON(ctx, "https://api.github.com/user/emails", token, &emails) == nil {
			for _, e := range emails {
				if e.Primary {
					p.Email, p.Verified = strings.ToLower(e.Email), e.Verified
				}
			}
		}
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider %s", name)
}

// oauthUser finds the user for a provider identity, links it to an existing
// account with the same verified email, or creates a new user.
func (s *authSvc) oauthUser(ctx context.Context, provider string, p *oauthProfileData) (*authUser, error) {
	if p.ID == "" {
		return nil, authError(400, "oauth_profile_failed", "provider returned no user id")
	}
	idData := map[string]any{"sub": p.ID, "provider_id": p.ID, "email": p.Email, "email_verified": p.Verified,
		"full_name": p.Name, "name": p.Name, "avatar_url": p.Avatar, "picture": p.Avatar, "iss": providerIssuer(provider)}
	idb, _ := json.Marshal(idData)

	var uid string
	if s.db.QueryRowContext(ctx, `SELECT user_id FROM auth.identities WHERE provider=$1 AND provider_id=$2`, provider, p.ID).Scan(&uid) == nil {
		s.db.ExecContext(ctx, `UPDATE auth.identities SET identity_data=$3, last_sign_in_at=now(), updated_at=now() WHERE provider=$1 AND provider_id=$2`, provider, p.ID, idb)
		return s.userByID(ctx, uid)
	}
	var u *authUser
	if p.Email != "" && p.Verified {
		if existing, err := s.userByEmail(ctx, p.Email); err == nil {
			u = existing
		}
	}
	if u == nil {
		if s.cfg.DisableSignup {
			return nil, authError(422, "signup_disabled", "Signups not allowed for this instance")
		}
		if p.Email == "" {
			return nil, authError(422, "email_required", "The provider did not return an email address")
		}
		meta := map[string]any{"full_name": p.Name, "name": p.Name, "avatar_url": p.Avatar, "email": p.Email, "email_verified": p.Verified, "provider_id": p.ID, "sub": p.ID}
		var err error
		if u, err = s.createUser(ctx, p.Email, "", meta, p.Verified, false); err != nil {
			return nil, err
		}
		s.db.ExecContext(ctx, `DELETE FROM auth.identities WHERE user_id=$1 AND provider='email'`, u.ID)
		am, _ := json.Marshal(map[string]any{"provider": provider, "providers": []string{provider}})
		s.db.ExecContext(ctx, `UPDATE auth.users SET raw_app_meta_data=$2 WHERE id=$1`, u.ID, am)
	} else {
		s.db.ExecContext(ctx, `UPDATE auth.users SET raw_app_meta_data = jsonb_set(raw_app_meta_data, '{providers}',
			(SELECT to_jsonb(array(SELECT DISTINCT jsonb_array_elements_text(coalesce(raw_app_meta_data->'providers','[]')) UNION SELECT $2::text)))) WHERE id=$1`, u.ID, provider)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth.identities(provider_id, user_id, identity_data, provider, email, last_sign_in_at) VALUES ($1,$2,$3,$4,$5,now())`,
		p.ID, u.ID, idb, provider, p.Email); err != nil {
		return nil, err
	}
	return s.userByID(ctx, u.ID)
}
