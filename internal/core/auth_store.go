package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// End-user authentication for projects (GoTrue-compatible, so supabase-js
// auth works). Users, sessions and tokens live in each project's own `auth`
// schema with Supabase's table and column names, so app SQL like
// `references auth.users(id)` and `auth.uid()` policies work unchanged.

const authSchemaSQL = `
CREATE TABLE IF NOT EXISTS auth.users (
	instance_id uuid,
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	aud text NOT NULL DEFAULT 'authenticated',
	role text NOT NULL DEFAULT 'authenticated',
	email text,
	encrypted_password text,
	email_confirmed_at timestamptz,
	invited_at timestamptz,
	confirmation_sent_at timestamptz,
	recovery_sent_at timestamptz,
	email_change text,
	email_change_sent_at timestamptz,
	last_sign_in_at timestamptz,
	raw_app_meta_data jsonb NOT NULL DEFAULT '{}',
	raw_user_meta_data jsonb NOT NULL DEFAULT '{}',
	is_super_admin boolean,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	phone text,
	phone_confirmed_at timestamptz,
	banned_until timestamptz,
	deleted_at timestamptz,
	is_anonymous boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_key ON auth.users (lower(email)) WHERE email IS NOT NULL;
CREATE TABLE IF NOT EXISTS auth.identities (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	provider_id text NOT NULL,
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	identity_data jsonb NOT NULL DEFAULT '{}',
	provider text NOT NULL,
	email text,
	last_sign_in_at timestamptz,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (provider_id, provider)
);
CREATE TABLE IF NOT EXISTS auth.sessions (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	aal text NOT NULL DEFAULT 'aal1',
	amr text NOT NULL DEFAULT 'password',
	user_agent text NOT NULL DEFAULT '',
	ip text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user ON auth.sessions(user_id);
CREATE TABLE IF NOT EXISTS auth.refresh_tokens (
	id bigserial PRIMARY KEY,
	token_hash text NOT NULL UNIQUE,
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	session_id uuid NOT NULL REFERENCES auth.sessions(id) ON DELETE CASCADE,
	revoked boolean NOT NULL DEFAULT false,
	parent_hash text,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS auth.one_time_tokens (
	id bigserial PRIMARY KEY,
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	token_type text NOT NULL,
	token_hash text NOT NULL UNIQUE,
	otp_hash text NOT NULL,
	relates_to text NOT NULL DEFAULT '',
	redirect_to text NOT NULL DEFAULT '',
	code_challenge text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS auth.flow_state (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	auth_code text NOT NULL UNIQUE,
	code_challenge text NOT NULL,
	code_challenge_method text NOT NULL,
	authentication_method text NOT NULL,
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	provider_token text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS auth.audit_log_entries (
	id bigserial PRIMARY KEY,
	payload jsonb NOT NULL,
	ip_address text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_at ON auth.audit_log_entries(created_at DESC);
-- v0.2: phone sign-in, MFA (TOTP)
ALTER TABLE auth.users ADD COLUMN IF NOT EXISTS phone_change text, ADD COLUMN IF NOT EXISTS phone_change_sent_at timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS users_phone_key ON auth.users (phone) WHERE phone IS NOT NULL AND phone <> '';
CREATE TABLE IF NOT EXISTS auth.mfa_factors (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id uuid NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
	friendly_name text NOT NULL DEFAULT '',
	factor_type text NOT NULL DEFAULT 'totp',
	status text NOT NULL DEFAULT 'unverified',
	secret text NOT NULL,
	last_used_step bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	last_challenged_at timestamptz
);
CREATE INDEX IF NOT EXISTS mfa_factors_user ON auth.mfa_factors(user_id);
CREATE TABLE IF NOT EXISTS auth.mfa_challenges (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	factor_id uuid NOT NULL REFERENCES auth.mfa_factors(id) ON DELETE CASCADE,
	created_at timestamptz NOT NULL DEFAULT now(),
	verified_at timestamptz,
	ip_address text NOT NULL DEFAULT ''
);
`

// ensureAuthSchema creates the auth tables as the project role (which owns
// the auth schema). The API roles get no access to these tables.
func (a *App) ensureAuthSchema(slug string) error {
	db, err := a.projectDB(slug)
	if err != nil {
		return err
	}
	if _, err := db.Exec(authSchemaSQL); err != nil {
		return fmt.Errorf("auth schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- settings

type SMTPConfig struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	Pass      string `json:"pass"`
	FromEmail string `json:"from_email"`
	FromName  string `json:"from_name"`
}

func (s SMTPConfig) configured() bool { return s.Host != "" && s.FromEmail != "" }

type OAuthProvider struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type EmailTemplate struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type AuthConfig struct {
	SiteURL           string                   `json:"site_url"`
	RedirectURLs      []string                 `json:"redirect_urls"`
	DisableSignup     bool                     `json:"disable_signup"`
	ConfirmEmail      bool                     `json:"confirm_email"`
	JWTExpiry         int                      `json:"jwt_expiry"`
	PasswordMinLength int                      `json:"password_min_length"`
	EmailRateLimit    int                      `json:"email_rate_limit_per_hour"`
	SMTP              SMTPConfig               `json:"smtp"`
	Google            OAuthProvider            `json:"google"`
	GitHub            OAuthProvider            `json:"github"`
	Apple             AppleProvider            `json:"apple"`
	Templates         map[string]EmailTemplate `json:"templates"`

	AnonymousUsers bool           `json:"anonymous_users"` // supabase.auth.signInAnonymously()
	Phone          PhoneConfig    `json:"phone"`
	MFA            MFAConfig      `json:"mfa"`
	OIDC           []OIDCProvider `json:"oidc"` // generic OpenID Connect providers (Keycloak, Auth0, Okta, Azure AD, ...)
}

// PhoneConfig: sign-in with a phone number and a one-time SMS code.
type PhoneConfig struct {
	Enabled     bool              `json:"enabled"`
	Autoconfirm bool              `json:"autoconfirm"` // phone + password sign-ups don't need a code first
	Provider    string            `json:"provider"`    // "" (codes only shown in the SMS log, for development) | "twilio" | "webhook"
	TwilioSID   string            `json:"twilio_account_sid"`
	TwilioToken string            `json:"twilio_auth_token"`
	TwilioFrom  string            `json:"twilio_from"` // sender number, or
	TwilioMSID  string            `json:"twilio_messaging_service_sid"`
	WebhookURL  string            `json:"webhook_url"` // POSTs {phone, otp, message}; signed like database webhooks
	WebhookKey  string            `json:"webhook_secret"`
	Template    string            `json:"template"`   // default "Your code is {{ .Code }}"
	OTPExpiry   int               `json:"otp_expiry"` // seconds, default 300
	TestOTP     map[string]string `json:"test_otp"`   // phone -> fixed code, never sent (store reviews, tests)
}

// MFAConfig: TOTP authenticator apps. On by default, like Supabase.
type MFAConfig struct {
	DisableTOTP bool   `json:"disable_totp"`
	Issuer      string `json:"issuer"` // shown in authenticator apps; default: the site URL's host
	MaxFactors  int    `json:"max_factors"`
}

var defaultTemplates = map[string]EmailTemplate{
	"confirmation": {Subject: "Confirm your signup", Body: `<h2>Confirm your signup</h2>
<p>Follow this link to confirm your account:</p>
<p><a href="{{ .ConfirmationURL }}">Confirm your email</a></p>
<p>Or enter this code: <strong>{{ .Token }}</strong></p>`},
	"magic_link": {Subject: "Your sign-in link", Body: `<h2>Sign in</h2>
<p><a href="{{ .ConfirmationURL }}">Sign in to {{ .SiteURL }}</a></p>
<p>Or enter this code: <strong>{{ .Token }}</strong></p>`},
	"recovery": {Subject: "Reset your password", Body: `<h2>Reset password</h2>
<p><a href="{{ .ConfirmationURL }}">Reset your password</a></p>
<p>Or enter this code: <strong>{{ .Token }}</strong></p>`},
	"invite": {Subject: "You have been invited", Body: `<h2>You have been invited</h2>
<p>You have been invited to create an account on {{ .SiteURL }}.</p>
<p><a href="{{ .ConfirmationURL }}">Accept the invite</a></p>`},
	"email_change": {Subject: "Confirm your new email", Body: `<h2>Confirm email change</h2>
<p>Confirm the change of your email from {{ .Email }} to {{ .NewEmail }}:</p>
<p><a href="{{ .ConfirmationURL }}">Change email</a></p>
<p>Or enter this code: <strong>{{ .Token }}</strong></p>`},
}

func (a *App) authConfig(slug string) AuthConfig {
	c := AuthConfig{
		SiteURL: "http://localhost:3000", JWTExpiry: 3600, PasswordMinLength: 6, EmailRateLimit: 30,
		RedirectURLs: []string{}, Templates: map[string]EmailTemplate{},
	}
	var enc string
	if a.Meta.QueryRow(`SELECT config_enc FROM auth_config WHERE project=$1`, slug).Scan(&enc) == nil {
		if raw, err := unseal(a.masterKey, enc); err == nil {
			json.Unmarshal([]byte(raw), &c)
		}
	}
	if c.JWTExpiry < 300 {
		c.JWTExpiry = 3600
	}
	if c.PasswordMinLength < 6 {
		c.PasswordMinLength = 6
	}
	if c.EmailRateLimit <= 0 {
		c.EmailRateLimit = 30
	}
	if c.RedirectURLs == nil {
		c.RedirectURLs = []string{}
	}
	if c.Templates == nil {
		c.Templates = map[string]EmailTemplate{}
	}
	if c.SMTP.Port == 0 {
		c.SMTP.Port = 587
	}
	if c.Phone.OTPExpiry < 60 || c.Phone.OTPExpiry > 3600 {
		c.Phone.OTPExpiry = 300
	}
	if c.Phone.TestOTP == nil {
		c.Phone.TestOTP = map[string]string{}
	}
	if c.MFA.MaxFactors <= 0 {
		c.MFA.MaxFactors = 10
	}
	if c.OIDC == nil {
		c.OIDC = []OIDCProvider{}
	}
	return c
}

func (a *App) saveAuthConfig(slug string, c AuthConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = a.Meta.Exec(`INSERT INTO auth_config(project, config_enc) VALUES ($1,$2)
		ON CONFLICT (project) DO UPDATE SET config_enc=$2`, slug, seal(a.masterKey, string(b)))
	return err
}

func (c AuthConfig) template(kind string) EmailTemplate {
	t := defaultTemplates[kind]
	if o, ok := c.Templates[kind]; ok {
		if o.Subject != "" {
			t.Subject = o.Subject
		}
		if o.Body != "" {
			t.Body = o.Body
		}
	}
	return t
}

// redirectAllowed prevents open redirects: only the site URL and the
// configured redirect URLs (a trailing * matches any suffix) are allowed.
func (c AuthConfig) redirectAllowed(to string) bool {
	if to == "" {
		return true
	}
	allowed := append([]string{c.SiteURL}, c.RedirectURLs...)
	for _, p := range allowed {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(to, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if to == p || strings.HasPrefix(to, strings.TrimRight(p, "/")+"/") || strings.HasPrefix(to, p+"?") || strings.HasPrefix(to, p+"#") {
			return true
		}
	}
	return false
}

func (c AuthConfig) redirectOrSite(to string) string {
	if to != "" && c.redirectAllowed(to) {
		return to
	}
	return c.SiteURL
}

// ---------------------------------------------------------------- email

type mailData struct {
	ConfirmationURL string
	Token           string
	TokenHash       string
	SiteURL         string
	Email           string
	NewEmail        string
	RedirectTo      string
	Data            map[string]any
}

// sendAuthMail renders a template and sends it via the project's SMTP server.
// Every message is also kept in the mailbox (auth_mails) so development works
// without SMTP: links can be opened from the UI.
func (a *App) sendAuthMail(ctx context.Context, slug string, cfg AuthConfig, kind, to string, d mailData) error {
	var sent int
	a.Meta.QueryRow(`SELECT count(*) FROM auth_mails WHERE project=$1 AND at > now() - interval '1 hour'`, slug).Scan(&sent)
	if sent >= cfg.EmailRateLimit {
		return &APIError{Status: 429, Code: "over_email_send_rate_limit", Message: "email rate limit exceeded"}
	}
	t := cfg.template(kind)
	d.SiteURL = cfg.SiteURL
	var subj, body bytes.Buffer
	if tt, err := template.New("s").Parse(t.Subject); err == nil {
		tt.Execute(&subj, d)
	} else {
		subj.WriteString(t.Subject)
	}
	bt, err := template.New("b").Parse(t.Body)
	if err != nil {
		return fmt.Errorf("email template %s: %w", kind, err)
	}
	if err := bt.Execute(&body, d); err != nil {
		return fmt.Errorf("email template %s: %w", kind, err)
	}
	delivered, errText := false, ""
	if cfg.SMTP.configured() {
		if err := smtpSend(cfg.SMTP, to, subj.String(), body.String()); err != nil {
			errText = err.Error()
		} else {
			delivered = true
		}
	} else {
		errText = "no SMTP configured - kept in the mailbox only"
	}
	a.Meta.Exec(`INSERT INTO auth_mails(project,to_addr,subject,body,kind,delivered,error,link) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		slug, to, subj.String(), body.String(), kind, delivered, errText, d.ConfirmationURL)
	if cfg.SMTP.configured() && !delivered {
		return &APIError{Status: 500, Code: "email_send_failed", Message: "Error sending email", Hint: errText}
	}
	return nil
}

func smtpSend(c SMTPConfig, to, subject, html string) error {
	from := c.FromEmail
	header := ""
	if c.FromName != "" {
		header = fmt.Sprintf("From: %q <%s>\r\n", c.FromName, from)
	} else {
		header = "From: " + from + "\r\n"
	}
	msg := header + "To: " + to + "\r\nSubject: " + mimeHeader(subject) + "\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nDate: " +
		time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + html
	addr := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	var auth smtp.Auth
	if c.User != "" {
		auth = smtp.PlainAuth("", c.User, c.Pass, c.Host)
	}
	if c.Port == 465 { // implicit TLS
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: c.Host})
		if err != nil {
			return err
		}
		cl, err := smtp.NewClient(conn, c.Host)
		if err != nil {
			return err
		}
		defer cl.Close()
		if auth != nil {
			if err := cl.Auth(auth); err != nil {
				return err
			}
		}
		if err := cl.Mail(from); err != nil {
			return err
		}
		if err := cl.Rcpt(to); err != nil {
			return err
		}
		w, err := cl.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(msg)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return cl.Quit()
	}
	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg)) // STARTTLS when offered
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + b64std(s) + "?="
		}
	}
	return s
}

// ---------------------------------------------------------------- audit

func authAudit(ctx context.Context, db *sql.DB, ip, action, actor, email string, extra map[string]any) {
	p := map[string]any{"action": action, "actor_id": actor, "actor_username": email, "timestamp": time.Now().UTC()}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	db.ExecContext(ctx, `INSERT INTO auth.audit_log_entries(payload, ip_address) VALUES ($1,$2)`, b, ip)
}
