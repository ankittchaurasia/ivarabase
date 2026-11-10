package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Paths reached by plain browser navigation (email links, OAuth redirects)
// carry no apikey header.
func authPublicPath(method, sub string) bool {
	if method == http.MethodPost && sub == "/callback" { // Sign in with Apple posts its result
		return true
	}
	return method == http.MethodGet && (sub == "/verify" || sub == "/authorize" || sub == "/callback" || sub == "/health" || sub == "/settings")
}

type authReq struct {
	*authSvc
	w      http.ResponseWriter
	r      *http.Request
	ip, ua string
}

// serveAuth handles /proj/<slug>/auth/v1/<sub>.
func (a *App) serveAuth(w http.ResponseWriter, r *http.Request, slug, sub string) {
	if err := a.ensureProjectAPIOnce(slug); err != nil {
		writeAuthError(w, err)
		return
	}
	s, err := a.authService(slug)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	x := &authReq{authSvc: s, w: w, r: r, ip: a.clientIP(r), ua: r.UserAgent()}
	ctx := r.Context()
	m := r.Method
	switch {
	case sub == "/health":
		writeJSON(w, 200, map[string]any{"name": "GoTrue", "version": "ivarabase-" + Version, "description": "IvaraBase auth (GoTrue compatible)"})
	case sub == "/settings":
		writeJSON(w, 200, map[string]any{
			"external":       s.cfg.externalProviders(),
			"disable_signup": s.cfg.DisableSignup, "mailer_autoconfirm": !s.cfg.ConfirmEmail, "phone_autoconfirm": s.cfg.Phone.Autoconfirm,
			"sms_provider": s.cfg.Phone.Provider, "saml_enabled": false, "mfa_enabled": !s.cfg.MFA.DisableTOTP,
		})
	case sub == "/signup" && m == "POST":
		x.respond(x.signup(ctx))
	case sub == "/token" && m == "POST":
		x.respond(x.token(ctx))
	case sub == "/user" && m == "GET":
		u, _, err := x.requireUser(ctx)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		writeJSON(w, 200, s.userJSON(ctx, u))
	case sub == "/user" && m == "PUT":
		x.respond(x.updateUser(ctx))
	case sub == "/logout" && m == "POST":
		if err := x.logout(ctx); err != nil {
			writeAuthError(w, err)
			return
		}
		w.WriteHeader(204)
	case sub == "/recover" && m == "POST":
		x.respond(x.recover(ctx))
	case sub == "/otp" && m == "POST":
		x.respond(x.otp(ctx))
	case sub == "/resend" && m == "POST":
		x.respond(x.resend(ctx))
	case sub == "/verify" && m == "POST":
		x.respond(x.verifyPost(ctx))
	case sub == "/verify" && m == "GET":
		x.verifyLink(ctx)
	case sub == "/authorize" && m == "GET":
		x.authorize(ctx)
	case sub == "/callback" && (m == "GET" || m == "POST"):
		x.callback(ctx)
	case sub == "/invite" && m == "POST":
		x.respond(x.invite(ctx))
	case sub == "/factors" || strings.HasPrefix(sub, "/factors/"):
		x.serveFactors(ctx, sub)
	case strings.HasPrefix(sub, "/admin/"):
		x.admin(ctx, strings.TrimPrefix(sub, "/admin"))
	default:
		writeAuthError(w, authError(404, "not_found", "unknown auth endpoint "+m+" "+sub))
	}
}

func (x *authReq) respond(v any, err error) {
	if err != nil {
		writeAuthError(x.w, err)
		return
	}
	writeJSON(x.w, 200, v)
}

func (x *authReq) bearerClaims() (map[string]any, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(x.r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return nil, authError(401, "no_authorization", "This endpoint requires a Bearer token")
	}
	c, err := verifyJWT(x.secret, tok)
	if err != nil {
		return nil, authError(403, "bad_jwt", "invalid JWT: unable to parse or verify signature, "+err.Error())
	}
	return c, nil
}

func (x *authReq) requireUser(ctx context.Context) (*authUser, map[string]any, error) {
	c, err := x.bearerClaims()
	if err != nil {
		return nil, nil, err
	}
	sub, _ := c["sub"].(string)
	if sub == "" || c["role"] == roleAnon || c["role"] == roleService {
		return nil, nil, authError(403, "bad_jwt", "invalid claim: missing sub claim")
	}
	if sid, _ := c["session_id"].(string); sid != "" {
		var ok bool
		x.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth.sessions WHERE id=$1)`, sid).Scan(&ok)
		if !ok {
			return nil, nil, authError(403, "session_not_found", "Session from session_id claim in JWT does not exist")
		}
	}
	u, err := x.userByID(ctx, sub)
	if err != nil {
		return nil, nil, authError(403, "user_not_found", "User from sub claim in JWT does not exist")
	}
	return u, c, nil
}

func (x *authReq) redirectParam(body map[string]any) string {
	if v := x.r.URL.Query().Get("redirect_to"); v != "" {
		return v
	}
	if o := obj(body, "options"); o != nil {
		if v := str(o, "email_redirect_to"); v != "" {
			return v
		}
		if v := str(o, "redirect_to"); v != "" {
			return v
		}
	}
	return str(body, "redirect_to")
}

func (x *authReq) checkRedirect(to string) (string, error) {
	if to == "" {
		return "", nil
	}
	if !x.cfg.redirectAllowed(to) {
		return "", authError(400, "validation_failed", "redirect_to is not in the allowed redirect URLs for this project")
	}
	return to, nil
}

// ---------------------------------------------------------------- endpoints

func (x *authReq) signup(ctx context.Context) (any, error) {
	if !authLimiter.allow(x.slug+"|signup|"+x.ip, 30, time.Hour) {
		return nil, authError(429, "over_request_rate_limit", "Request rate limit reached")
	}
	if x.cfg.DisableSignup {
		return nil, authError(422, "signup_disabled", "Signups not allowed for this instance")
	}
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	switch {
	case str(body, "email") == "" && str(body, "phone") == "":
		return x.signupAnonymous(ctx, body)
	case str(body, "phone") != "" && str(body, "email") == "":
		return x.signupPhone(ctx, body)
	}
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	pw := str(body, "password")
	if err := x.checkPassword(pw); err != nil {
		return nil, err
	}
	redirect, err := x.checkRedirect(x.redirectParam(body))
	if err != nil {
		return nil, err
	}
	challenge := pkceChallenge(str(body, "code_challenge"), str(body, "code_challenge_method"))
	meta := obj(body, "data")

	if existing, err := x.userByEmail(ctx, email); err == nil {
		if existing.EmailConfirmedAt.Valid {
			if !x.cfg.ConfirmEmail {
				return nil, authError(422, "user_already_exists", "User already registered")
			}
			// don't reveal that the address is registered
			return map[string]any{"id": randUUID(), "aud": "authenticated", "role": "", "email": email, "phone": "",
				"confirmation_sent_at": time.Now(), "app_metadata": map[string]any{"provider": "email", "providers": []string{"email"}},
				"user_metadata": map[string]any{}, "identities": []any{}, "created_at": time.Now(), "updated_at": time.Now(), "is_anonymous": false}, nil
		}
		h, _ := hashPW(pw)
		x.db.ExecContext(ctx, `UPDATE auth.users SET encrypted_password=$2, updated_at=now() WHERE id=$1`, existing.ID, h)
		if _, err := x.sendToken(ctx, existing, "confirmation", email, "", redirect, challenge); err != nil {
			return nil, err
		}
		u, _ := x.userByID(ctx, existing.ID)
		return x.userJSON(ctx, u), nil
	}

	u, err := x.createUser(ctx, email, pw, meta, !x.cfg.ConfirmEmail, false)
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "user_signedup", u.ID, email, nil)
	if x.cfg.ConfirmEmail {
		if _, err := x.sendToken(ctx, u, "confirmation", email, "", redirect, challenge); err != nil {
			return nil, err
		}
		u, _ = x.userByID(ctx, u.ID)
		return x.userJSON(ctx, u), nil
	}
	return x.issueSession(ctx, u, "password", x.ua, x.ip)
}

func (x *authReq) token(ctx context.Context) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	switch x.r.URL.Query().Get("grant_type") {
	case "password":
		if !authLimiter.allow(x.slug+"|token|"+x.ip, 30, 5*time.Minute) {
			return nil, authError(429, "over_request_rate_limit", "Request rate limit reached")
		}
		if str(body, "email") == "" && str(body, "phone") != "" {
			return x.passwordPhone(ctx, body)
		}
		email := strings.ToLower(strings.TrimSpace(str(body, "email")))
		bad := authError(400, "invalid_credentials", "Invalid login credentials")
		u, err := x.userByEmail(ctx, email)
		if err != nil || !u.EncPW.Valid {
			return nil, bad
		}
		if !checkHash(u.EncPW.String, str(body, "password")) {
			authAudit(ctx, x.db, x.ip, "login_failed", u.ID, email, nil)
			return nil, bad
		}
		if !u.EmailConfirmedAt.Valid && x.cfg.ConfirmEmail {
			return nil, authError(400, "email_not_confirmed", "Email not confirmed")
		}
		authAudit(ctx, x.db, x.ip, "login", u.ID, email, map[string]any{"provider": "email"})
		return x.issueSession(ctx, u, "password", x.ua, x.ip)
	case "refresh_token":
		rt := str(body, "refresh_token")
		if rt == "" {
			return nil, authError(400, "validation_failed", "refresh_token required")
		}
		return x.refresh(ctx, rt)
	case "pkce":
		return x.exchangePKCE(ctx, str(body, "auth_code"), str(body, "code_verifier"), x.ua, x.ip)
	case "id_token":
		return x.idTokenGrant(ctx, body)
	default:
		return nil, authError(400, "unsupported_grant_type", "unsupported_grant_type")
	}
}

func checkHash(hash, pw string) bool {
	return bcryptCompare(hash, pw)
}

func (x *authReq) updateUser(ctx context.Context) (any, error) {
	u, _, err := x.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	if pw, ok := body["password"].(string); ok && pw != "" {
		if err := x.checkPassword(pw); err != nil {
			return nil, err
		}
		h, _ := hashPW(pw)
		x.db.ExecContext(ctx, `UPDATE auth.users SET encrypted_password=$2, updated_at=now() WHERE id=$1`, u.ID, h)
		authAudit(ctx, x.db, x.ip, "user_updated_password", u.ID, u.Email.String, nil)
	}
	if d := obj(body, "data"); d != nil {
		b, _ := json.Marshal(d)
		x.db.ExecContext(ctx, `UPDATE auth.users SET raw_user_meta_data = raw_user_meta_data || $2::jsonb, updated_at=now() WHERE id=$1`, u.ID, string(b))
	}
	if e, ok := body["email"].(string); ok && e != "" && !strings.EqualFold(e, u.Email.String) {
		email, err := normEmail(e)
		if err != nil {
			return nil, err
		}
		if _, err := x.userByEmail(ctx, email); err == nil {
			return nil, authError(422, "email_exists", "A user with this email address has already been registered")
		}
		if x.cfg.ConfirmEmail {
			x.db.ExecContext(ctx, `UPDATE auth.users SET email_change=$2 WHERE id=$1`, u.ID, email)
			if _, err := x.sendToken(ctx, u, "email_change", email, email, x.redirectParam(body), ""); err != nil {
				return nil, err
			}
		} else {
			x.db.ExecContext(ctx, `UPDATE auth.users SET email=$2, email_confirmed_at=coalesce(email_confirmed_at, now()), updated_at=now() WHERE id=$1`, u.ID, email)
			x.db.ExecContext(ctx, `UPDATE auth.identities SET email=$2 WHERE user_id=$1 AND provider='email'`, u.ID, email)
			x.promote(ctx, u.ID, "email")
		}
	}
	if p, ok := body["phone"].(string); ok && p != "" {
		if err := x.updatePhone(ctx, u, p); err != nil {
			return nil, err
		}
	}
	u, err = x.userByID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return x.userJSON(ctx, u), nil
}

func (x *authReq) logout(ctx context.Context) error {
	u, c, err := x.requireUser(ctx)
	if err != nil {
		return err
	}
	sid, _ := c["session_id"].(string)
	switch x.r.URL.Query().Get("scope") {
	case "local":
		x.db.ExecContext(ctx, `DELETE FROM auth.sessions WHERE id=$1`, sid)
	case "others":
		x.db.ExecContext(ctx, `DELETE FROM auth.sessions WHERE user_id=$1 AND id<>$2`, u.ID, sid)
	default:
		x.db.ExecContext(ctx, `DELETE FROM auth.sessions WHERE user_id=$1`, u.ID)
	}
	authAudit(ctx, x.db, x.ip, "logout", u.ID, u.Email.String, nil)
	return nil
}

func (x *authReq) recover(ctx context.Context) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	redirect, err := x.checkRedirect(x.redirectParam(body))
	if err != nil {
		return nil, err
	}
	if !authLimiter.allow(x.slug+"|recover|"+email, 3, time.Hour) {
		return nil, authError(429, "over_email_send_rate_limit", "For security purposes, you can only request this after some time")
	}
	if u, err := x.userByEmail(ctx, email); err == nil {
		if _, err := x.sendToken(ctx, u, "recovery", email, "", redirect, pkceChallenge(str(body, "code_challenge"), str(body, "code_challenge_method"))); err != nil {
			return nil, err
		}
		authAudit(ctx, x.db, x.ip, "user_recovery_requested", u.ID, email, nil)
	}
	return map[string]any{}, nil // same answer whether or not the address exists
}

func (x *authReq) otp(ctx context.Context) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	if str(body, "phone") != "" {
		return x.otpPhone(ctx, body)
	}
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	redirect, err := x.checkRedirect(x.redirectParam(body))
	if err != nil {
		return nil, err
	}
	if !authLimiter.allow(x.slug+"|otp|"+email, 5, time.Hour) {
		return nil, authError(429, "over_email_send_rate_limit", "For security purposes, you can only request this after some time")
	}
	challenge := pkceChallenge(str(body, "code_challenge"), str(body, "code_challenge_method"))
	u, err := x.userByEmail(ctx, email)
	if err != nil {
		create := true
		if v, ok := body["create_user"].(bool); ok {
			create = v
		}
		if !create || x.cfg.DisableSignup {
			return nil, authError(422, "otp_disabled", "Signups not allowed for otp")
		}
		if u, err = x.createUser(ctx, email, "", obj(body, "data"), false, false); err != nil {
			return nil, err
		}
		authAudit(ctx, x.db, x.ip, "user_signedup", u.ID, email, map[string]any{"via": "otp"})
	}
	kind := "magic_link"
	if !u.EmailConfirmedAt.Valid {
		kind = "confirmation"
	}
	if _, err := x.sendToken(ctx, u, kind, email, "", redirect, challenge); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

func (x *authReq) resend(ctx context.Context) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	if t := str(body, "type"); t == "sms" || t == "phone_change" {
		return x.resendPhone(ctx, body)
	}
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	if !authLimiter.allow(x.slug+"|resend|"+email, 3, time.Hour) {
		return nil, authError(429, "over_email_send_rate_limit", "For security purposes, you can only request this after some time")
	}
	redirect, _ := x.checkRedirect(x.redirectParam(body))
	switch str(body, "type") {
	case "signup":
		if u, err := x.userByEmail(ctx, email); err == nil && !u.EmailConfirmedAt.Valid {
			if _, err := x.sendToken(ctx, u, "confirmation", email, "", redirect, ""); err != nil {
				return nil, err
			}
		}
	case "email_change":
		if u, err := scanUser(x.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE lower(email_change)=lower($1)`, email)); err == nil {
			if _, err := x.sendToken(ctx, u, "email_change", email, email, redirect, ""); err != nil {
				return nil, err
			}
		}
	default:
		return nil, authError(400, "validation_failed", "type must be signup or email_change")
	}
	return map[string]any{}, nil
}

func (x *authReq) verifyPost(ctx context.Context) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	if t := str(body, "type"); (t == "sms" || t == "phone_change") && str(body, "phone") != "" {
		return x.verifyPhone(ctx, body)
	}
	if str(body, "email") != "" && !authLimiter.allow(x.slug+"|verify|"+strings.ToLower(str(body, "email")), 10, 15*time.Minute) {
		return nil, authError(429, "over_request_rate_limit", "Too many attempts, request a new code later")
	}
	v, err := x.consume(ctx, str(body, "type"), str(body, "token_hash"), strings.ToLower(str(body, "email")), str(body, "token"))
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "user_confirmation_requested", v.user.ID, v.user.Email.String, map[string]any{"type": v.kind})
	return x.issueSession(ctx, v.user, "otp", x.ua, x.ip)
}

// verifyLink handles email link clicks: GET /verify?token=&type=&redirect_to=
func (x *authReq) verifyLink(ctx context.Context) {
	q := x.r.URL.Query()
	redirect := x.cfg.redirectOrSite(q.Get("redirect_to"))
	v, err := x.consume(ctx, q.Get("type"), q.Get("token"), "", "")
	if err != nil {
		ae := AsAPIError(err)
		x.redirectFragment(redirect, url.Values{"error": {"access_denied"}, "error_code": {ae.Code}, "error_description": {ae.Message}})
		return
	}
	if v.redirect != "" {
		redirect = x.cfg.redirectOrSite(v.redirect)
	}
	x.finishBrowserLogin(ctx, v.user, linkType[v.kind], "otp", v.challenge, "", redirect)
}

// finishBrowserLogin completes a redirect-based sign-in: a PKCE code in the
// query string when the client started with a code challenge, otherwise the
// session in the URL fragment (implicit flow), exactly like GoTrue.
func (x *authReq) finishBrowserLogin(ctx context.Context, u *authUser, typ, method, challenge, providerToken, redirect string) {
	if challenge != "" {
		code, err := x.newFlow(ctx, u.ID, challenge, method, providerToken)
		if err != nil {
			x.redirectFragment(redirect, url.Values{"error": {"server_error"}, "error_description": {err.Error()}})
			return
		}
		sep := "?"
		if strings.Contains(redirect, "?") {
			sep = "&"
		}
		http.Redirect(x.w, x.r, redirect+sep+"code="+url.QueryEscape(code), http.StatusSeeOther)
		return
	}
	sess, err := x.issueSession(ctx, u, method, x.ua, x.ip)
	if err != nil {
		ae := AsAPIError(err)
		x.redirectFragment(redirect, url.Values{"error": {"access_denied"}, "error_code": {ae.Code}, "error_description": {ae.Message}})
		return
	}
	f := url.Values{
		"access_token": {sess["access_token"].(string)}, "refresh_token": {sess["refresh_token"].(string)},
		"expires_in": {strconv.Itoa(x.cfg.JWTExpiry)}, "expires_at": {fmt.Sprint(sess["expires_at"])},
		"token_type": {"bearer"}, "type": {typ},
	}
	if providerToken != "" {
		f.Set("provider_token", providerToken)
	}
	x.redirectFragment(redirect, f)
}

func (x *authReq) redirectFragment(to string, f url.Values) {
	if to == "" {
		writeAuthError(x.w, authError(400, f.Get("error_code"), f.Get("error_description")))
		return
	}
	if i := strings.Index(to, "#"); i >= 0 {
		to = to[:i]
	}
	http.Redirect(x.w, x.r, to+"#"+f.Encode(), http.StatusSeeOther)
}

func (x *authReq) invite(ctx context.Context) (any, error) {
	if err := x.requireService(); err != nil {
		return nil, err
	}
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	u, err := x.inviteUser(ctx, str(body, "email"), obj(body, "data"), x.redirectParam(body))
	if err != nil {
		return nil, err
	}
	return x.userJSON(ctx, u), nil
}

func (s *authSvc) inviteUser(ctx context.Context, email string, data map[string]any, redirect string) (*authUser, error) {
	email, err := normEmail(email)
	if err != nil {
		return nil, err
	}
	u, err := s.userByEmail(ctx, email)
	if err == nil && u.EmailConfirmedAt.Valid {
		return nil, authError(422, "email_exists", "A user with this email address has already been registered")
	}
	if err != nil {
		if u, err = s.createUser(ctx, email, "", data, false, true); err != nil {
			return nil, err
		}
	}
	if _, err := s.sendToken(ctx, u, "invite", email, "", s.cfg.redirectOrSite(redirect), ""); err != nil {
		return nil, err
	}
	return s.userByID(ctx, u.ID)
}

// ---------------------------------------------------------------- admin (service_role)

func (x *authReq) requireService() error {
	c, err := x.bearerClaims()
	if err != nil {
		return err
	}
	if c["role"] != roleService {
		return authError(403, "not_admin", "User not allowed")
	}
	return nil
}

func (x *authReq) admin(ctx context.Context, sub string) {
	if err := x.requireService(); err != nil {
		writeAuthError(x.w, err)
		return
	}
	m := x.r.Method
	switch {
	case sub == "/users" && m == "GET":
		page, _ := strconv.Atoi(x.r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(x.r.URL.Query().Get("per_page"))
		users, total, err := x.listUsers(ctx, page, per, "")
		if err != nil {
			writeAuthError(x.w, err)
			return
		}
		x.w.Header().Set("X-Total-Count", strconv.Itoa(total))
		writeJSON(x.w, 200, map[string]any{"users": users, "aud": "authenticated", "total": total})
	case sub == "/users" && m == "POST":
		body, err := readJSON(x.r)
		if err != nil {
			writeAuthError(x.w, err)
			return
		}
		x.respond(x.adminCreate(ctx, body))
	case strings.HasPrefix(sub, "/users/") && strings.Contains(strings.TrimPrefix(sub, "/users/"), "/factors"):
		// auth.admin.mfa.listFactors / deleteFactor
		parts := strings.Split(strings.TrimPrefix(sub, "/users/"), "/")
		if !uuidLike(parts[0]) || parts[1] != "factors" {
			writeAuthError(x.w, authError(404, "not_found", "unknown admin endpoint"))
			return
		}
		switch {
		case len(parts) == 2 && m == "GET":
			writeJSON(x.w, 200, x.factorsJSON(ctx, parts[0]))
		case len(parts) == 3 && m == "DELETE" && uuidLike(parts[2]):
			res, err := x.db.ExecContext(ctx, `DELETE FROM auth.mfa_factors WHERE id=$1 AND user_id=$2`, parts[2], parts[0])
			if n, _ := res.RowsAffected(); err != nil || n == 0 {
				writeAuthError(x.w, authError(404, "mfa_factor_not_found", "Factor not found"))
				return
			}
			writeJSON(x.w, 200, map[string]any{"id": parts[2]})
		default:
			writeAuthError(x.w, authError(404, "not_found", "unknown admin endpoint"))
		}
	case strings.HasPrefix(sub, "/users/"):
		id := strings.TrimPrefix(sub, "/users/")
		if strings.Contains(id, "/") || !uuidLike(id) {
			writeAuthError(x.w, authError(404, "user_not_found", "User not found"))
			return
		}
		switch m {
		case "GET":
			u, err := x.userByID(ctx, id)
			if err != nil {
				writeAuthError(x.w, authError(404, "user_not_found", "User not found"))
				return
			}
			writeJSON(x.w, 200, x.userJSON(ctx, u))
		case "PUT":
			body, err := readJSON(x.r)
			if err != nil {
				writeAuthError(x.w, err)
				return
			}
			x.respond(x.adminUpdate(ctx, id, body))
		case "DELETE":
			if err := x.deleteUser(ctx, id); err != nil {
				writeAuthError(x.w, err)
				return
			}
			writeJSON(x.w, 200, map[string]any{})
		default:
			writeAuthError(x.w, authError(405, "method_not_allowed", "method not allowed"))
		}
	case sub == "/generate_link" && m == "POST":
		body, err := readJSON(x.r)
		if err != nil {
			writeAuthError(x.w, err)
			return
		}
		x.respond(x.generateLink(ctx, body))
	default:
		writeAuthError(x.w, authError(404, "not_found", "unknown admin endpoint"))
	}
}

func uuidLike(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

func randUUID() string {
	b := randBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *authSvc) listUsers(ctx context.Context, page, per int, search string) ([]map[string]any, int, error) {
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 1000 {
		per = 50
	}
	where := `deleted_at IS NULL`
	args := []any{}
	if search != "" {
		where += ` AND (email ILIKE $1 OR id::text = $2 OR phone LIKE $1)`
		args = append(args, "%"+search+"%", search)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM auth.users WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+userCols+` FROM auth.users WHERE %s ORDER BY created_at DESC LIMIT %d OFFSET %d`, where, per, (page-1)*per), args...)
	if err != nil {
		return nil, 0, err
	}
	var us []*authUser
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		us = append(us, u)
	}
	rows.Close()
	out := []map[string]any{}
	for _, u := range us {
		out = append(out, s.userJSON(ctx, u))
	}
	return out, total, nil
}

func (s *authSvc) adminCreate(ctx context.Context, body map[string]any) (any, error) {
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	pw := str(body, "password")
	if pw != "" {
		if err := s.checkPassword(pw); err != nil {
			return nil, err
		}
	}
	confirmed, _ := body["email_confirm"].(bool)
	meta := obj(body, "user_metadata")
	if meta == nil {
		meta = obj(body, "data")
	}
	u, err := s.createUser(ctx, email, pw, meta, confirmed, false)
	if err != nil {
		return nil, err
	}
	if am := obj(body, "app_metadata"); am != nil {
		b, _ := json.Marshal(am)
		s.db.ExecContext(ctx, `UPDATE auth.users SET raw_app_meta_data = raw_app_meta_data || $2::jsonb WHERE id=$1`, u.ID, string(b))
	}
	if d := str(body, "ban_duration"); d != "" && d != "none" {
		if err := s.setBan(ctx, u.ID, d); err != nil {
			return nil, err
		}
	}
	u, _ = s.userByID(ctx, u.ID)
	return s.userJSON(ctx, u), nil
}

func (s *authSvc) adminUpdate(ctx context.Context, id string, body map[string]any) (any, error) {
	u, err := s.userByID(ctx, id)
	if err != nil {
		return nil, authError(404, "user_not_found", "User not found")
	}
	if e := str(body, "email"); e != "" && !strings.EqualFold(e, u.Email.String) {
		email, err := normEmail(e)
		if err != nil {
			return nil, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE auth.users SET email=$2, updated_at=now() WHERE id=$1`, id, email); err != nil {
			return nil, authError(422, "email_exists", "A user with this email address has already been registered")
		}
		s.db.ExecContext(ctx, `UPDATE auth.identities SET email=$2 WHERE user_id=$1 AND provider='email'`, id, email)
	}
	if pw := str(body, "password"); pw != "" {
		if err := s.checkPassword(pw); err != nil {
			return nil, err
		}
		h, _ := hashPW(pw)
		s.db.ExecContext(ctx, `UPDATE auth.users SET encrypted_password=$2, updated_at=now() WHERE id=$1`, id, h)
	}
	if c, ok := body["email_confirm"].(bool); ok && c {
		s.db.ExecContext(ctx, `UPDATE auth.users SET email_confirmed_at=coalesce(email_confirmed_at, now()) WHERE id=$1`, id)
	}
	if m := obj(body, "user_metadata"); m != nil {
		b, _ := json.Marshal(m)
		s.db.ExecContext(ctx, `UPDATE auth.users SET raw_user_meta_data = raw_user_meta_data || $2::jsonb, updated_at=now() WHERE id=$1`, id, string(b))
	}
	if m := obj(body, "app_metadata"); m != nil {
		b, _ := json.Marshal(m)
		s.db.ExecContext(ctx, `UPDATE auth.users SET raw_app_meta_data = raw_app_meta_data || $2::jsonb, updated_at=now() WHERE id=$1`, id, string(b))
	}
	if d := str(body, "ban_duration"); d != "" {
		if err := s.setBan(ctx, id, d); err != nil {
			return nil, err
		}
	}
	u, _ = s.userByID(ctx, id)
	return s.userJSON(ctx, u), nil
}

// setBan accepts "none" or a Go duration like "24h", "876000h".
func (s *authSvc) setBan(ctx context.Context, id, d string) error {
	if d == "none" {
		_, err := s.db.ExecContext(ctx, `UPDATE auth.users SET banned_until=NULL, updated_at=now() WHERE id=$1`, id)
		return err
	}
	dur, err := time.ParseDuration(d)
	if err != nil || dur <= 0 {
		return authError(400, "validation_failed", "ban_duration must be 'none' or a duration like 24h")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE auth.users SET banned_until=$2, updated_at=now() WHERE id=$1`, id, time.Now().Add(dur)); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM auth.sessions WHERE user_id=$1`, id)
	return err
}

func (s *authSvc) deleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth.users WHERE id=$1`, id)
	if err != nil {
		return sqlError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return authError(404, "user_not_found", "User not found")
	}
	return nil
}

func (s *authSvc) generateLink(ctx context.Context, body map[string]any) (any, error) {
	typ := str(body, "type")
	kind := map[string]string{"signup": "confirmation", "magiclink": "magic_link", "recovery": "recovery", "invite": "invite"}[typ]
	if kind == "" {
		return nil, authError(400, "validation_failed", "type must be signup, magiclink, recovery or invite")
	}
	email, err := normEmail(str(body, "email"))
	if err != nil {
		return nil, err
	}
	redirect := s.cfg.redirectOrSite(str(body, "redirect_to"))
	if o := obj(body, "options"); o != nil && str(o, "redirect_to") != "" {
		redirect = s.cfg.redirectOrSite(str(o, "redirect_to"))
	}
	u, err := s.userByEmail(ctx, email)
	if err != nil {
		if typ == "recovery" || typ == "magiclink" {
			return nil, authError(404, "user_not_found", "User with this email not found")
		}
		if u, err = s.createUser(ctx, email, str(body, "password"), obj(body, "data"), false, typ == "invite"); err != nil {
			return nil, err
		}
	}
	t, err := s.newToken(ctx, u, kind, "", redirect, "")
	if err != nil {
		return nil, err
	}
	out := s.userJSON(ctx, u)
	props := map[string]any{"action_link": t.Link, "email_otp": t.OTP, "hashed_token": t.TokenHash, "redirect_to": redirect, "verification_type": typ}
	for k, v := range props {
		out[k] = v
	}
	out["properties"] = props
	return out, nil
}
