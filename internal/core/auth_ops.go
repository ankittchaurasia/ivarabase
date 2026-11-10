package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Operations for managing a project's end users and auth settings from the
// UI, CLI and MCP (the same things the service_role admin API can do).

const masked = "********"

func maskAuthConfig(c AuthConfig) AuthConfig {
	if c.SMTP.Pass != "" {
		c.SMTP.Pass = masked
	}
	if c.Google.ClientSecret != "" {
		c.Google.ClientSecret = masked
	}
	if c.GitHub.ClientSecret != "" {
		c.GitHub.ClientSecret = masked
	}
	if c.Apple.PrivateKey != "" {
		c.Apple.PrivateKey = masked
	}
	if c.Apple.BundleIDs == nil {
		c.Apple.BundleIDs = []string{}
	}
	if c.Phone.TwilioToken != "" {
		c.Phone.TwilioToken = masked
	}
	if c.Phone.WebhookKey != "" {
		c.Phone.WebhookKey = masked
	}
	oidc := make([]OIDCProvider, len(c.OIDC))
	for i, p := range c.OIDC {
		if p.ClientSecret != "" {
			p.ClientSecret = masked
		}
		oidc[i] = p
	}
	c.OIDC = oidc
	return c
}

func registerAuthOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	userParam := Param{Name: "id", Type: "string", Required: true, Desc: "user id (uuid)"}
	svc := func(c *Ctx, a Args) (*authSvc, error) {
		if err := c.App.ensureProjectAPIOnce(a.Str("project")); err != nil {
			return nil, err
		}
		return c.App.authService(a.Str("project"))
	}

	r.add(&Op{Name: "auth.users_list", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/users", Level: LevelRead,
		Summary: "List the project's app users (newest first)",
		Params:  []Param{projectParam, {Name: "search", Type: "string", Desc: "email contains, or exact id"}, {Name: "page", Type: "integer", Default: 1}, {Name: "per_page", Type: "integer", Default: 50}},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			users, total, err := s.listUsers(c, a.Int("page", 1), a.Int("per_page", 50), a.Str("search"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"users": users, "total": total}, nil
		}})

	r.add(&Op{Name: "auth.user_get", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/users/{id}", Level: LevelRead,
		Summary: "One app user with identities and active sessions",
		Params:  []Param{projectParam, userParam},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			u, err := s.userByID(c, a.Str("id"))
			if err != nil {
				return nil, errNotFound("user_not_found", "no user "+a.Str("id"), "")
			}
			out := s.userJSON(c, u)
			sessions := []map[string]any{}
			rows, err := s.db.QueryContext(c, `SELECT id, created_at, updated_at, amr, user_agent, ip FROM auth.sessions WHERE user_id=$1 ORDER BY updated_at DESC`, u.ID)
			if err == nil {
				for rows.Next() {
					var id, amr, ua, ip string
					var cr, up time.Time
					rows.Scan(&id, &cr, &up, &amr, &ua, &ip)
					sessions = append(sessions, map[string]any{"id": id, "created_at": cr, "last_active_at": up, "method": amr, "user_agent": ua, "ip": ip})
				}
				rows.Close()
			}
			out["sessions"] = sessions
			return out, nil
		}})

	r.add(&Op{Name: "auth.user_create", Tag: "auth", Method: "POST", Path: "/api/v1/projects/{project}/auth/users", Level: LevelWrite,
		Summary: "Create an app user (optionally already confirmed)",
		Params: []Param{projectParam, {Name: "email", Type: "string", Required: true}, {Name: "password", Type: "string", Secret: true, Desc: "omit for a passwordless (magic link / OAuth) user"},
			{Name: "email_confirm", Type: "boolean", Default: true}, {Name: "user_metadata", Type: "object"}},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			return s.adminCreate(c, map[string]any{"email": a.Str("email"), "password": a.Str("password"), "email_confirm": a.Bool("email_confirm", true), "user_metadata": a.Obj("user_metadata")})
		}})

	r.add(&Op{Name: "auth.user_update", Tag: "auth", Method: "PATCH", Path: "/api/v1/projects/{project}/auth/users/{id}", Level: LevelWrite,
		Summary: "Change an app user's email, password, metadata, confirmation or ban",
		Params: []Param{projectParam, userParam, {Name: "email", Type: "string"}, {Name: "password", Type: "string", Secret: true},
			{Name: "email_confirm", Type: "boolean"}, {Name: "user_metadata", Type: "object"}, {Name: "app_metadata", Type: "object"},
			{Name: "ban_duration", Type: "string", Desc: "'none' to unban, or a duration like 24h / 876000h (signs the user out)"}},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			body := map[string]any{}
			for _, k := range []string{"email", "password", "ban_duration"} {
				if v := a.Str(k); v != "" {
					body[k] = v
				}
			}
			if _, ok := a["email_confirm"]; ok {
				body["email_confirm"] = a.Bool("email_confirm", false)
			}
			if m := a.Obj("user_metadata"); m != nil {
				body["user_metadata"] = m
			}
			if m := a.Obj("app_metadata"); m != nil {
				body["app_metadata"] = m
			}
			return s.adminUpdate(c, a.Str("id"), body)
		}})

	r.add(&Op{Name: "auth.user_delete", Tag: "auth", Method: "DELETE", Path: "/api/v1/projects/{project}/auth/users/{id}", Level: LevelWrite,
		Destructive: true,
		Summary:     "Delete an app user, their identities and sessions",
		Params:      []Param{projectParam, userParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			u, err := s.userByID(c, a.Str("id"))
			if err != nil {
				return nil, errNotFound("user_not_found", "no user "+a.Str("id"), "")
			}
			return map[string]any{"user": u.Email.String, "id": u.ID, "created_at": u.CreatedAt,
				"note": "rows in your own tables that reference auth.users follow their foreign key rules (e.g. on delete cascade)"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": a.Str("id")}, s.deleteUser(c, a.Str("id"))
		}})

	r.add(&Op{Name: "auth.user_signout", Tag: "auth", Method: "POST", Path: "/api/v1/projects/{project}/auth/users/{id}/signout", Level: LevelWrite,
		Summary: "End all of an app user's sessions (refresh tokens stop working)",
		Params:  []Param{projectParam, userParam},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			res, err := s.db.ExecContext(c, `DELETE FROM auth.sessions WHERE user_id=$1`, a.Str("id"))
			if err != nil {
				return nil, err
			}
			n, _ := res.RowsAffected()
			return map[string]any{"sessions_ended": n}, nil
		}})

	r.add(&Op{Name: "auth.send_link", Tag: "auth", Method: "POST", Path: "/api/v1/projects/{project}/auth/send-link", Level: LevelWrite,
		Summary: "Email a user a magic link, password recovery link or invite",
		Params: []Param{projectParam, {Name: "email", Type: "string", Required: true},
			{Name: "type", Type: "string", Required: true, Enum: []string{"magiclink", "recovery", "invite"}},
			{Name: "redirect_to", Type: "string"}},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			redirect := s.cfg.redirectOrSite(a.Str("redirect_to"))
			if a.Str("type") == "invite" {
				u, err := s.inviteUser(c, a.Str("email"), nil, redirect)
				if err != nil {
					return nil, err
				}
				return map[string]any{"sent": "invite", "to": u.Email.String}, nil
			}
			u, err := s.userByEmail(c, strings.ToLower(a.Str("email")))
			if err != nil {
				return nil, errNotFound("user_not_found", "no user with that email", "use type=invite to add someone new")
			}
			kind := map[string]string{"magiclink": "magic_link", "recovery": "recovery"}[a.Str("type")]
			if kind == "magic_link" && !u.EmailConfirmedAt.Valid {
				kind = "confirmation"
			}
			if _, err := s.sendToken(c, u, kind, u.Email.String, "", redirect, ""); err != nil {
				return nil, err
			}
			return map[string]any{"sent": a.Str("type"), "to": u.Email.String}, nil
		}})

	r.add(&Op{Name: "auth.config_get", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/config", Level: LevelRead,
		Summary: "Auth settings: URLs, sign-up rules, SMTP, OAuth providers, email templates (secrets masked)",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			if _, err := c.App.project(a.Str("project")); err != nil {
				return nil, err
			}
			cfg := c.App.authConfig(a.Str("project"))
			tpl := map[string]EmailTemplate{}
			for k := range defaultTemplates {
				tpl[k] = cfg.template(k)
			}
			cfg.Templates = tpl
			return map[string]any{"config": maskAuthConfig(cfg), "callback_url": c.App.projectURL(a.Str("project")) + "/auth/v1/callback",
				"auth_url": c.App.projectURL(a.Str("project")) + "/auth/v1"}, nil
		}})

	r.add(&Op{Name: "auth.config_set", Tag: "auth", Method: "PATCH", Path: "/api/v1/projects/{project}/auth/config", Level: LevelWrite,
		Summary: "Change auth settings (only the fields you send)",
		Params: []Param{projectParam,
			{Name: "site_url", Type: "string", Desc: "your app's URL; default redirect after sign-in"},
			{Name: "redirect_urls", Type: "array", Desc: "other allowed redirect URLs; a trailing * matches anything after it"},
			{Name: "disable_signup", Type: "boolean"},
			{Name: "confirm_email", Type: "boolean", Desc: "require email confirmation before password sign-in"},
			{Name: "jwt_expiry", Type: "integer", Desc: "access token lifetime in seconds (300-604800)"},
			{Name: "password_min_length", Type: "integer"},
			{Name: "email_rate_limit_per_hour", Type: "integer"},
			{Name: "smtp_host", Type: "string"}, {Name: "smtp_port", Type: "integer"}, {Name: "smtp_user", Type: "string"},
			{Name: "smtp_pass", Type: "string", Secret: true}, {Name: "smtp_from_email", Type: "string"}, {Name: "smtp_from_name", Type: "string"},
			{Name: "google_enabled", Type: "boolean"}, {Name: "google_client_id", Type: "string"}, {Name: "google_client_secret", Type: "string", Secret: true},
			{Name: "github_enabled", Type: "boolean"}, {Name: "github_client_id", Type: "string"}, {Name: "github_client_secret", Type: "string", Secret: true},
			{Name: "apple_enabled", Type: "boolean"},
			{Name: "apple_client_id", Type: "string", Desc: "Services ID for web sign-in, e.g. com.example.web"},
			{Name: "apple_team_id", Type: "string", Desc: "10-character Apple Developer Team ID"},
			{Name: "apple_key_id", Type: "string", Desc: "ID of the Sign in with Apple key"},
			{Name: "apple_private_key", Type: "string", Secret: true, Desc: "contents of AuthKey_<KEYID>.p8; IvaraBase signs the client secret itself, nothing to renew"},
			{Name: "apple_bundle_ids", Type: "array", Desc: "iOS/macOS bundle IDs allowed to sign in with signInWithIdToken (native apps)"},
			{Name: "templates", Type: "object", Desc: "{confirmation|magic_link|recovery|invite|email_change: {subject, body}}; Go/Supabase template syntax, e.g. {{ .ConfirmationURL }}, {{ .Token }}"},
			{Name: "anonymous_users", Type: "boolean", Desc: "allow supabase.auth.signInAnonymously()"},
			{Name: "phone_enabled", Type: "boolean", Desc: "sign-in with phone number + SMS code"},
			{Name: "phone_autoconfirm", Type: "boolean", Desc: "phone + password sign-ups don't need a code first"},
			{Name: "sms_provider", Type: "string", Enum: []string{"", "twilio", "webhook"}, Desc: "empty = codes only appear in the SMS log (development)"},
			{Name: "twilio_account_sid", Type: "string"}, {Name: "twilio_auth_token", Type: "string", Secret: true},
			{Name: "twilio_from", Type: "string", Desc: "sender number, e.g. +15551234567"}, {Name: "twilio_messaging_service_sid", Type: "string"},
			{Name: "sms_webhook_url", Type: "string", Desc: "for any other SMS provider: receives POST {phone, otp, message}, signed with sms_webhook_secret"},
			{Name: "sms_webhook_secret", Type: "string", Secret: true},
			{Name: "sms_template", Type: "string", Desc: "default: Your code is {{ .Code }}"},
			{Name: "sms_otp_expiry", Type: "integer", Desc: "seconds a code stays valid (60-3600, default 300)"},
			{Name: "sms_test_otp", Type: "object", Desc: "{phone: code} fixed codes that are never sent (app store review, tests)"},
			{Name: "mfa_totp_enabled", Type: "boolean", Desc: "authenticator-app MFA (supabase.auth.mfa); on by default"},
			{Name: "mfa_issuer", Type: "string", Desc: "name shown in authenticator apps"},
			{Name: "oidc_providers", Type: "array", Items: "object", Desc: "replaces the list: [{name, label, enabled, issuer, client_id, client_secret, scopes}]; send client_secret as ******** to keep the saved one"}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			cfg := c.App.authConfig(slug)
			has := func(k string) bool { _, ok := a[k]; return ok }
			setS := func(k string, dst *string) {
				if has(k) {
					if v := strings.TrimSpace(a.Str(k)); v != masked {
						*dst = v
					}
				}
			}
			setS("site_url", &cfg.SiteURL)
			if has("redirect_urls") {
				cfg.RedirectURLs = a.StrList("redirect_urls")
			}
			if has("disable_signup") {
				cfg.DisableSignup = a.Bool("disable_signup", false)
			}
			if has("confirm_email") {
				cfg.ConfirmEmail = a.Bool("confirm_email", false)
			}
			if has("jwt_expiry") {
				cfg.JWTExpiry = clamp(a.Int("jwt_expiry", 3600), 300, 604800)
			}
			if has("password_min_length") {
				cfg.PasswordMinLength = clamp(a.Int("password_min_length", 6), 6, 72)
			}
			if has("email_rate_limit_per_hour") {
				cfg.EmailRateLimit = clamp(a.Int("email_rate_limit_per_hour", 30), 1, 10000)
			}
			setS("smtp_host", &cfg.SMTP.Host)
			if has("smtp_port") {
				cfg.SMTP.Port = a.Int("smtp_port", 587)
			}
			setS("smtp_user", &cfg.SMTP.User)
			setS("smtp_pass", &cfg.SMTP.Pass)
			setS("smtp_from_email", &cfg.SMTP.FromEmail)
			setS("smtp_from_name", &cfg.SMTP.FromName)
			if has("google_enabled") {
				cfg.Google.Enabled = a.Bool("google_enabled", false)
			}
			setS("google_client_id", &cfg.Google.ClientID)
			setS("google_client_secret", &cfg.Google.ClientSecret)
			if has("github_enabled") {
				cfg.GitHub.Enabled = a.Bool("github_enabled", false)
			}
			setS("github_client_id", &cfg.GitHub.ClientID)
			setS("github_client_secret", &cfg.GitHub.ClientSecret)
			if has("apple_enabled") {
				cfg.Apple.Enabled = a.Bool("apple_enabled", false)
			}
			setS("apple_client_id", &cfg.Apple.ClientID)
			setS("apple_team_id", &cfg.Apple.TeamID)
			setS("apple_key_id", &cfg.Apple.KeyID)
			if has("apple_private_key") && strings.TrimSpace(a.Str("apple_private_key")) != masked {
				if v := strings.TrimSpace(a.Str("apple_private_key")); v == "" {
					cfg.Apple.PrivateKey = ""
				} else if norm, _, err := normalizeAppleKey(v); err != nil {
					return nil, errBad("invalid_parameter", "apple_private_key: "+err.Error(), "")
				} else {
					cfg.Apple.PrivateKey = norm
				}
			}
			if has("apple_bundle_ids") {
				cfg.Apple.BundleIDs = nil
				for _, b := range a.StrList("apple_bundle_ids") {
					if b = strings.TrimSpace(b); b != "" {
						cfg.Apple.BundleIDs = append(cfg.Apple.BundleIDs, b)
					}
				}
			}
			if t := a.Obj("templates"); t != nil {
				for kind, v := range t {
					if _, ok := defaultTemplates[kind]; !ok {
						return nil, errBad("invalid_parameter", "unknown template "+kind, "templates: confirmation, magic_link, recovery, invite, email_change")
					}
					m, _ := v.(map[string]any)
					tpl := cfg.Templates[kind]
					if s, ok := m["subject"].(string); ok {
						tpl.Subject = s
					}
					if s, ok := m["body"].(string); ok {
						tpl.Body = s
					}
					cfg.Templates[kind] = tpl
				}
			}
			if has("anonymous_users") {
				cfg.AnonymousUsers = a.Bool("anonymous_users", false)
			}
			if has("phone_enabled") {
				cfg.Phone.Enabled = a.Bool("phone_enabled", false)
			}
			if has("phone_autoconfirm") {
				cfg.Phone.Autoconfirm = a.Bool("phone_autoconfirm", false)
			}
			setS("sms_provider", &cfg.Phone.Provider)
			setS("twilio_account_sid", &cfg.Phone.TwilioSID)
			setS("twilio_auth_token", &cfg.Phone.TwilioToken)
			setS("twilio_from", &cfg.Phone.TwilioFrom)
			setS("twilio_messaging_service_sid", &cfg.Phone.TwilioMSID)
			setS("sms_webhook_url", &cfg.Phone.WebhookURL)
			setS("sms_webhook_secret", &cfg.Phone.WebhookKey)
			if has("sms_template") {
				cfg.Phone.Template = a.Str("sms_template")
			}
			if has("sms_otp_expiry") {
				cfg.Phone.OTPExpiry = clamp(a.Int("sms_otp_expiry", 300), 60, 3600)
			}
			if t := a.Obj("sms_test_otp"); has("sms_test_otp") {
				cfg.Phone.TestOTP = map[string]string{}
				for ph, v := range t {
					n, err := normPhone(ph)
					code, _ := v.(string)
					if err != nil || len(code) < 4 || len(code) > 10 {
						return nil, errBad("invalid_parameter", "sms_test_otp: use {\"+15551234567\": \"123456\"}", "")
					}
					cfg.Phone.TestOTP[n] = code
				}
			}
			switch cfg.Phone.Provider {
			case "", "twilio", "webhook":
			default:
				return nil, errBad("invalid_parameter", "sms_provider must be twilio, webhook or empty", "")
			}
			if cfg.Phone.Provider == "webhook" && cfg.Phone.WebhookURL == "" {
				return nil, errBad("invalid_parameter", "the webhook SMS provider needs sms_webhook_url", "")
			}
			if has("mfa_totp_enabled") {
				cfg.MFA.DisableTOTP = !a.Bool("mfa_totp_enabled", true)
			}
			setS("mfa_issuer", &cfg.MFA.Issuer)
			if has("oidc_providers") {
				raw, _ := json.Marshal(a["oidc_providers"])
				var list []OIDCProvider
				if err := json.Unmarshal(raw, &list); err != nil {
					return nil, errBad("invalid_parameter", "oidc_providers: "+err.Error(), "")
				}
				for i := range list {
					list[i].Name = strings.ToLower(strings.TrimSpace(list[i].Name))
					list[i].Issuer = strings.TrimRight(strings.TrimSpace(list[i].Issuer), "/")
					if list[i].ClientSecret == masked {
						if old, ok := func() (OIDCProvider, bool) {
							for _, o := range cfg.OIDC {
								if o.Name == list[i].Name {
									return o, true
								}
							}
							return OIDCProvider{}, false
						}(); ok {
							list[i].ClientSecret = old.ClientSecret
						} else {
							list[i].ClientSecret = ""
						}
					}
				}
				if err := validateOIDC(list); err != nil {
					return nil, err
				}
				cfg.OIDC = list
			}
			if cfg.Google.Enabled && (cfg.Google.ClientID == "" || cfg.Google.ClientSecret == "") {
				return nil, errBad("invalid_parameter", "Google needs a client id and secret before it can be enabled", "")
			}
			if cfg.GitHub.Enabled && (cfg.GitHub.ClientID == "" || cfg.GitHub.ClientSecret == "") {
				return nil, errBad("invalid_parameter", "GitHub needs a client id and secret before it can be enabled", "")
			}
			if err := checkAppleConfig(cfg.Apple); err != nil {
				return nil, err
			}
			if err := c.App.saveAuthConfig(slug, cfg); err != nil {
				return nil, err
			}
			return c.App.Ops.Get("auth.config_get").Run(c, Args{"project": slug})
		}})

	r.add(&Op{Name: "auth.test_email", Tag: "auth", Method: "POST", Path: "/api/v1/projects/{project}/auth/test-email", Level: LevelWrite,
		Summary: "Send a test email with the project's SMTP settings",
		Params:  []Param{projectParam, {Name: "to", Type: "string", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := c.App.authConfig(a.Str("project"))
			if !cfg.SMTP.configured() {
				return nil, errBad("smtp_not_configured", "set smtp_host and smtp_from_email first", "")
			}
			if err := smtpSend(cfg.SMTP, a.Str("to"), "IvaraBase test email", "<p>Your SMTP settings work.</p>"); err != nil {
				return nil, &APIError{Status: 502, Code: "email_send_failed", Message: err.Error()}
			}
			return map[string]any{"sent": true, "to": a.Str("to")}, nil
		}})

	r.add(&Op{Name: "auth.sms_list", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/sms", Level: LevelWrite,
		Summary:     "SMS log: every sign-in code the project sent (codes are visible without a provider, so it needs write access)",
		Description: "Without an SMS provider, codes are only kept here - read them from the UI during development.",
		Params:      []Param{projectParam, {Name: "limit", Type: "integer", Default: 50}},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(fmt.Sprintf(`SELECT id, at, to_phone, body, kind, delivered, error FROM auth_sms WHERE project=$1 ORDER BY at DESC LIMIT %d`,
				clamp(a.Int("limit", 50), 1, 500)), a.Str("project"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var at time.Time
				var to, body, kind, errText string
				var delivered bool
				rows.Scan(&id, &at, &to, &body, &kind, &delivered, &errText)
				out = append(out, map[string]any{"id": id, "at": at, "to": "+" + to, "body": body, "kind": kind, "delivered": delivered, "error": errText})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "auth.user_mfa_reset", Tag: "auth", Method: "DELETE", Path: "/api/v1/projects/{project}/auth/users/{id}/factors", Level: LevelWrite, Destructive: true,
		Summary: "Remove a user's MFA factors (when they lost their authenticator) and sign them out",
		Params:  []Param{projectParam, userParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			return map[string]any{"would_remove_factors": s.factorsJSON(c, a.Str("id")), "and_sign_out": a.Str("id")}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			if !uuidLike(a.Str("id")) {
				return nil, errNotFound("user_not_found", "no user with that id", "")
			}
			res, err := s.db.ExecContext(c, `DELETE FROM auth.mfa_factors WHERE user_id=$1`, a.Str("id"))
			if err != nil {
				return nil, err
			}
			n, _ := res.RowsAffected()
			s.db.ExecContext(c, `DELETE FROM auth.sessions WHERE user_id=$1`, a.Str("id"))
			return map[string]any{"removed": n}, nil
		}})

	r.add(&Op{Name: "auth.mails_list", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/mails", Level: LevelWrite,
		Summary:     "Mailbox: every auth email the project sent (links work, so it needs write access)",
		Description: "Without SMTP configured, emails are only kept here - open the links from the UI during development.",
		Params:      []Param{projectParam, {Name: "limit", Type: "integer", Default: 50}},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(fmt.Sprintf(`SELECT id, at, to_addr, subject, body, kind, delivered, error, link FROM auth_mails WHERE project=$1 ORDER BY at DESC LIMIT %d`,
				clamp(a.Int("limit", 50), 1, 500)), a.Str("project"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var at time.Time
				var to, subj, body, kind, errText, link string
				var delivered bool
				rows.Scan(&id, &at, &to, &subj, &body, &kind, &delivered, &errText, &link)
				out = append(out, map[string]any{"id": id, "at": at, "to": to, "subject": subj, "body": body, "kind": kind,
					"delivered": delivered, "error": errText, "link": link})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "auth.logs", Tag: "auth", Method: "GET", Path: "/api/v1/projects/{project}/auth/logs", Level: LevelRead,
		Summary: "Auth events: sign-ups, logins, failed logins, recoveries, logouts",
		Params:  []Param{projectParam, {Name: "limit", Type: "integer", Default: 100}},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := svc(c, a)
			if err != nil {
				return nil, err
			}
			rows, err := s.db.QueryContext(c, fmt.Sprintf(`SELECT id, payload, ip_address, created_at FROM auth.audit_log_entries ORDER BY created_at DESC LIMIT %d`, clamp(a.Int("limit", 100), 1, 1000)))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var p []byte
				var ip string
				var at time.Time
				rows.Scan(&id, &p, &ip, &at)
				out = append(out, map[string]any{"id": id, "payload": json.RawMessage(p), "ip": ip, "at": at})
			}
			return out, rows.Err()
		}})
}

// ensureProjectAPIOnce makes sure the auth schema exists before management ops
// touch it (projects created before auth existed get it on first use).
var authReady sync.Map // slug -> true once the auth schema is known to exist

func (a *App) ensureProjectAPIOnce(slug string) error {
	if _, ok := authReady.Load(slug); ok {
		return nil
	}
	if _, err := a.project(slug); err != nil {
		return err
	}
	db, err := a.projectDB(slug)
	if err != nil {
		return err
	}
	var ok bool
	db.QueryRow(`SELECT to_regclass('auth.users') IS NOT NULL AND to_regclass('storage.objects') IS NOT NULL`).Scan(&ok)
	if ok {
		authReady.Store(slug, true)
		return nil
	}
	return a.ensureProjectAPI(slug)
}
