package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Phone sign-in (signUp / signInWithPassword / signInWithOtp / verifyOtp with
// `phone`) and anonymous users (signInAnonymously), matching GoTrue.
//
// Codes go out through Twilio, or a webhook for any other SMS provider (the
// body is signed: X-Ivarabase-Signature: sha256=<hex HMAC-SHA256 of
// "<timestamp>.<body>"> with X-Ivarabase-Timestamp), or - with no provider -
// only into the project's SMS log, for development. Phones are stored like
// GoTrue does: digits only, no "+".

func normPhone(p string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(p) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", authError(400, "validation_failed", "Invalid phone number format (E.164 required)")
		}
	}
	s := b.String()
	if len(s) < 8 || len(s) > 15 || s[0] == '0' {
		return "", authError(400, "validation_failed", "Invalid phone number format (E.164 required)")
	}
	return s, nil
}

func (s *authSvc) userByPhone(ctx context.Context, phone string) (*authUser, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE phone=$1 AND deleted_at IS NULL`, phone))
}

func (s *authSvc) requirePhone() error {
	if !s.cfg.Phone.Enabled {
		return authError(422, "phone_provider_disabled", "Phone signups are disabled")
	}
	return nil
}

// createPhoneUser inserts a user with a phone identity.
func (s *authSvc) createPhoneUser(ctx context.Context, phone, password string, meta map[string]any, confirmed bool) (*authUser, error) {
	var hash any
	if password != "" {
		h, err := hashPW(password)
		if err != nil {
			return nil, err
		}
		hash = h
	}
	if meta == nil {
		meta = map[string]any{}
	}
	um, _ := json.Marshal(meta)
	am, _ := json.Marshal(map[string]any{"provider": "phone", "providers": []string{"phone"}})
	var confirmedAt any
	if confirmed {
		confirmedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `INSERT INTO auth.users(phone, encrypted_password, phone_confirmed_at, raw_app_meta_data, raw_user_meta_data)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+userCols, phone, hash, confirmedAt, am, um))
	if err != nil {
		if strings.Contains(err.Error(), "users_phone_key") {
			return nil, authError(422, "phone_exists", "Phone number already registered by another user")
		}
		return nil, err
	}
	idData, _ := json.Marshal(map[string]any{"sub": u.ID, "phone": phone, "phone_verified": confirmed, "email_verified": false})
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth.identities(provider_id, user_id, identity_data, provider) VALUES ($1,$2,$3,'phone')`, u.ID, u.ID, idData); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// anonymous: POST /signup with neither email nor phone.
func (x *authReq) signupAnonymous(ctx context.Context, body map[string]any) (any, error) {
	if !x.cfg.AnonymousUsers {
		return nil, authError(422, "anonymous_provider_disabled", "Anonymous sign-ins are disabled")
	}
	if !authLimiter.allow(x.slug+"|anon|"+x.ip, 30, time.Hour) {
		return nil, authError(429, "over_request_rate_limit", "Request rate limit reached")
	}
	meta := obj(body, "data")
	if meta == nil {
		meta = map[string]any{}
	}
	um, _ := json.Marshal(meta)
	am, _ := json.Marshal(map[string]any{"provider": "anonymous", "providers": []string{"anonymous"}})
	u, err := scanUser(x.db.QueryRowContext(ctx, `INSERT INTO auth.users(is_anonymous, raw_app_meta_data, raw_user_meta_data) VALUES (true,$1,$2) RETURNING `+userCols, am, um))
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "user_signedup", u.ID, "", map[string]any{"provider": "anonymous"})
	return x.issueSession(ctx, u, "anonymous", x.ua, x.ip)
}

// promote turns an anonymous user into a permanent one once it has a
// confirmed email or phone (GoTrue's "convert an anonymous user").
func (s *authSvc) promote(ctx context.Context, uid, provider string) {
	s.db.ExecContext(ctx, `UPDATE auth.users SET is_anonymous=false,
		raw_app_meta_data = jsonb_set(jsonb_set(raw_app_meta_data, '{provider}', to_jsonb($2::text)), '{providers}',
		(SELECT to_jsonb(array(SELECT DISTINCT p FROM jsonb_array_elements_text(coalesce(raw_app_meta_data->'providers','[]')) p WHERE p <> 'anonymous' UNION SELECT $2::text)))),
		updated_at=now() WHERE id=$1 AND is_anonymous`, uid, provider)
	switch provider {
	case "email":
		s.db.ExecContext(ctx, `INSERT INTO auth.identities(provider_id, user_id, identity_data, provider, email)
			SELECT id, id, jsonb_build_object('sub', id::text, 'email', email, 'email_verified', email_confirmed_at IS NOT NULL), 'email', email
			FROM auth.users WHERE id=$1 AND email IS NOT NULL ON CONFLICT (provider_id, provider) DO UPDATE SET email=EXCLUDED.email, identity_data=EXCLUDED.identity_data`, uid)
	case "phone":
		s.db.ExecContext(ctx, `INSERT INTO auth.identities(provider_id, user_id, identity_data, provider)
			SELECT id, id, jsonb_build_object('sub', id::text, 'phone', phone, 'phone_verified', phone_confirmed_at IS NOT NULL), 'phone'
			FROM auth.users WHERE id=$1 AND phone IS NOT NULL ON CONFLICT (provider_id, provider) DO UPDATE SET identity_data=EXCLUDED.identity_data`, uid)
	}
}

// ---------------------------------------------------------------- SMS codes

var smsKinds = map[string]string{"phone_confirmation": "confirmation", "sms_otp": "otp", "phone_change": "phone_change"}

// sendPhoneOTP stores a new code for the user and sends it to phone.
func (s *authSvc) sendPhoneOTP(ctx context.Context, u *authUser, kind, phone string) (string, error) {
	if !authLimiter.allow(s.slug+"|sms-min|"+phone, 1, time.Minute) {
		return "", authError(429, "over_sms_send_rate_limit", "For security purposes, you can only request this after 60 seconds")
	}
	if !authLimiter.allow(s.slug+"|sms|"+phone, 5, time.Hour) {
		return "", authError(429, "over_sms_send_rate_limit", "SMS rate limit exceeded")
	}
	otp, test := s.cfg.Phone.TestOTP[phone]
	if !test {
		otp = randOTP()
	}
	s.db.ExecContext(ctx, `DELETE FROM auth.one_time_tokens WHERE user_id=$1 AND token_type=$2`, u.ID, kind)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth.one_time_tokens(user_id, token_type, token_hash, otp_hash, relates_to)
		VALUES ($1,$2,$3,$4,$5)`, u.ID, kind, sha256Hex(randHex(28)), sha256Hex(u.ID+":"+phone+":"+otp), phone); err != nil {
		return "", err
	}
	if kind == "phone_change" {
		s.db.ExecContext(ctx, `UPDATE auth.users SET phone_change_sent_at=now() WHERE id=$1`, u.ID)
	} else {
		s.db.ExecContext(ctx, `UPDATE auth.users SET confirmation_sent_at=now() WHERE id=$1`, u.ID)
	}
	id := randHex(12)
	if test {
		s.app.logSMS(s.slug, phone, "(test number: code not sent)", kind, true, "", id)
		return id, nil
	}
	return id, s.app.sendSMS(ctx, s.slug, s.cfg.Phone, kind, phone, otp, id)
}

func (a *App) logSMS(slug, phone, body, kind string, delivered bool, errText, id string) {
	a.Meta.Exec(`INSERT INTO auth_sms(project, to_phone, body, kind, delivered, error, message_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		slug, phone, body, kind, delivered, errText, id)
}

func (a *App) sendSMS(ctx context.Context, slug string, cfg PhoneConfig, kind, phone, otp, id string) error {
	tplText := cfg.Template
	if strings.TrimSpace(tplText) == "" {
		tplText = "Your code is {{ .Code }}"
	}
	var msg bytes.Buffer
	if t, err := template.New("sms").Parse(tplText); err == nil {
		t.Execute(&msg, map[string]string{"Code": otp, "Token": otp})
	} else {
		msg.WriteString("Your code is " + otp)
	}
	body := msg.String()
	var err error
	switch cfg.Provider {
	case "":
		a.logSMS(slug, phone, body, kind, false, "no SMS provider configured: the code is only shown here", id)
		return nil
	case "twilio":
		err = sendTwilio(ctx, cfg, phone, body)
	case "webhook":
		err = sendSMSWebhook(ctx, cfg, slug, kind, phone, otp, body)
	default:
		err = fmt.Errorf("unknown SMS provider %q", cfg.Provider)
	}
	if err != nil {
		a.logSMS(slug, phone, body, kind, false, err.Error(), id)
		return &APIError{Status: 500, Code: "sms_send_failed", Message: "Error sending SMS: " + err.Error()}
	}
	a.logSMS(slug, phone, body, kind, true, "", id)
	return nil
}

var smsClient = &http.Client{Timeout: 15 * time.Second}

func sendTwilio(ctx context.Context, cfg PhoneConfig, phone, body string) error {
	if cfg.TwilioSID == "" || cfg.TwilioToken == "" || (cfg.TwilioFrom == "" && cfg.TwilioMSID == "") {
		return fmt.Errorf("Twilio needs an account SID, auth token and a sender number or messaging service")
	}
	form := url.Values{"To": {"+" + phone}, "Body": {body}}
	if cfg.TwilioMSID != "" {
		form.Set("MessagingServiceSid", cfg.TwilioMSID)
	} else {
		form.Set("From", cfg.TwilioFrom)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.twilio.com/2010-04-01/Accounts/"+url.PathEscape(cfg.TwilioSID)+"/Messages.json", strings.NewReader(form.Encode()))
	req.SetBasicAuth(cfg.TwilioSID, cfg.TwilioToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := smsClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct{ Message string }
		json.Unmarshal(b, &e)
		return fmt.Errorf("Twilio: %s %s", resp.Status, e.Message)
	}
	return nil
}

// signBody returns the signature headers IvaraBase puts on outgoing webhooks.
func signBody(secret string, body []byte) (ts, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return ts, "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func sendSMSWebhook(ctx context.Context, cfg PhoneConfig, slug, kind, phone, otp, message string) error {
	if cfg.WebhookURL == "" {
		return fmt.Errorf("webhook_url is not set")
	}
	b, _ := json.Marshal(map[string]any{"type": "sms", "kind": smsKinds[kind], "project": slug, "phone": "+" + phone, "otp": otp, "message": message})
	req, _ := http.NewRequestWithContext(ctx, "POST", cfg.WebhookURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "IvaraBase/"+Version)
	if cfg.WebhookKey != "" {
		ts, sig := signBody(cfg.WebhookKey, b)
		req.Header.Set("X-Ivarabase-Timestamp", ts)
		req.Header.Set("X-Ivarabase-Signature", sig)
	}
	resp, err := smsClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("SMS webhook returned %s", resp.Status)
	}
	return nil
}

// consumePhone checks a 6-digit SMS code for phone (type sms or phone_change).
func (s *authSvc) consumePhone(ctx context.Context, typ, phone, otp string) (*authUser, error) {
	expired := authError(403, "otp_expired", "Token has expired or is invalid")
	if !authLimiter.allow(s.slug+"|verify|"+phone, 10, 15*time.Minute) {
		return nil, authError(429, "over_request_rate_limit", "Too many attempts, request a new code later")
	}
	var u *authUser
	var err error
	kinds := []string{"phone_confirmation", "sms_otp"}
	if typ == "phone_change" {
		kinds = []string{"phone_change"}
		u, err = scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE phone_change=$1 AND deleted_at IS NULL`, phone))
	} else {
		u, err = s.userByPhone(ctx, phone)
	}
	if err != nil {
		return nil, expired
	}
	var id int64
	var kind string
	var created time.Time
	if s.db.QueryRowContext(ctx, `SELECT id, token_type, created_at FROM auth.one_time_tokens WHERE user_id=$1 AND otp_hash=$2 AND token_type = ANY($3)`,
		u.ID, sha256Hex(u.ID+":"+phone+":"+strings.TrimSpace(otp)), "{"+strings.Join(kinds, ",")+"}").Scan(&id, &kind, &created) != nil {
		return nil, expired
	}
	if time.Since(created) > time.Duration(s.cfg.Phone.OTPExpiry)*time.Second {
		s.db.ExecContext(ctx, `DELETE FROM auth.one_time_tokens WHERE id=$1`, id)
		return nil, expired
	}
	s.db.ExecContext(ctx, `DELETE FROM auth.one_time_tokens WHERE id=$1`, id)
	if kind == "phone_change" {
		if _, err := s.db.ExecContext(ctx, `UPDATE auth.users SET phone=phone_change, phone_change=NULL, phone_confirmed_at=now(), updated_at=now() WHERE id=$1`, u.ID); err != nil {
			if strings.Contains(err.Error(), "users_phone_key") {
				return nil, authError(422, "phone_exists", "Phone number already registered by another user")
			}
			return nil, err
		}
		s.db.ExecContext(ctx, `UPDATE auth.identities SET identity_data = identity_data || jsonb_build_object('phone', $2::text, 'phone_verified', true), updated_at=now() WHERE user_id=$1 AND provider='phone'`, u.ID, phone)
	} else {
		s.db.ExecContext(ctx, `UPDATE auth.users SET phone_confirmed_at=coalesce(phone_confirmed_at, now()), updated_at=now() WHERE id=$1`, u.ID)
		s.db.ExecContext(ctx, `UPDATE auth.identities SET identity_data = identity_data || '{"phone_verified": true}' WHERE user_id=$1 AND provider='phone'`, u.ID)
	}
	s.promote(ctx, u.ID, "phone")
	return s.userByID(ctx, u.ID)
}

// ---------------------------------------------------------------- endpoints

func (x *authReq) signupPhone(ctx context.Context, body map[string]any) (any, error) {
	if err := x.requirePhone(); err != nil {
		return nil, err
	}
	phone, err := normPhone(str(body, "phone"))
	if err != nil {
		return nil, err
	}
	pw := str(body, "password")
	if err := x.checkPassword(pw); err != nil {
		return nil, err
	}
	if existing, err := x.userByPhone(ctx, phone); err == nil {
		if existing.PhoneConfirmedAt.Valid {
			return nil, authError(422, "user_already_exists", "User already registered")
		}
		h, _ := hashPW(pw)
		x.db.ExecContext(ctx, `UPDATE auth.users SET encrypted_password=$2, updated_at=now() WHERE id=$1`, existing.ID, h)
		if _, err := x.sendPhoneOTP(ctx, existing, "phone_confirmation", phone); err != nil {
			return nil, err
		}
		u, _ := x.userByID(ctx, existing.ID)
		return x.userJSON(ctx, u), nil
	}
	u, err := x.createPhoneUser(ctx, phone, pw, obj(body, "data"), x.cfg.Phone.Autoconfirm)
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "user_signedup", u.ID, "", map[string]any{"provider": "phone"})
	if x.cfg.Phone.Autoconfirm {
		return x.issueSession(ctx, u, "password", x.ua, x.ip)
	}
	if _, err := x.sendPhoneOTP(ctx, u, "phone_confirmation", phone); err != nil {
		return nil, err
	}
	u, _ = x.userByID(ctx, u.ID)
	return x.userJSON(ctx, u), nil
}

func (x *authReq) passwordPhone(ctx context.Context, body map[string]any) (any, error) {
	bad := authError(400, "invalid_credentials", "Invalid login credentials")
	phone, err := normPhone(str(body, "phone"))
	if err != nil {
		return nil, bad
	}
	u, err := x.userByPhone(ctx, phone)
	if err != nil || !u.EncPW.Valid {
		return nil, bad
	}
	if !checkHash(u.EncPW.String, str(body, "password")) {
		authAudit(ctx, x.db, x.ip, "login_failed", u.ID, "", map[string]any{"phone": phone})
		return nil, bad
	}
	if !u.PhoneConfirmedAt.Valid && !x.cfg.Phone.Autoconfirm {
		return nil, authError(400, "phone_not_confirmed", "Phone not confirmed")
	}
	authAudit(ctx, x.db, x.ip, "login", u.ID, "", map[string]any{"provider": "phone"})
	return x.issueSession(ctx, u, "password", x.ua, x.ip)
}

func (x *authReq) otpPhone(ctx context.Context, body map[string]any) (any, error) {
	if err := x.requirePhone(); err != nil {
		return nil, err
	}
	phone, err := normPhone(str(body, "phone"))
	if err != nil {
		return nil, err
	}
	u, err := x.userByPhone(ctx, phone)
	if err != nil {
		create := true
		if v, ok := body["create_user"].(bool); ok {
			create = v
		}
		if !create || x.cfg.DisableSignup {
			return nil, authError(422, "otp_disabled", "Signups not allowed for otp")
		}
		if u, err = x.createPhoneUser(ctx, phone, "", obj(body, "data"), false); err != nil {
			return nil, err
		}
		authAudit(ctx, x.db, x.ip, "user_signedup", u.ID, "", map[string]any{"via": "otp", "provider": "phone"})
	}
	kind := "sms_otp"
	if !u.PhoneConfirmedAt.Valid {
		kind = "phone_confirmation"
	}
	id, err := x.sendPhoneOTP(ctx, u, kind, phone)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message_id": id}, nil
}

func (x *authReq) verifyPhone(ctx context.Context, body map[string]any) (any, error) {
	phone, err := normPhone(str(body, "phone"))
	if err != nil {
		return nil, err
	}
	u, err := x.consumePhone(ctx, str(body, "type"), phone, str(body, "token"))
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "user_confirmation_requested", u.ID, "", map[string]any{"type": str(body, "type"), "provider": "phone"})
	return x.issueSession(ctx, u, "otp", x.ua, x.ip)
}

func (x *authReq) resendPhone(ctx context.Context, body map[string]any) (any, error) {
	if err := x.requirePhone(); err != nil {
		return nil, err
	}
	phone, err := normPhone(str(body, "phone"))
	if err != nil {
		return nil, err
	}
	var id string
	switch str(body, "type") {
	case "sms":
		if u, err := x.userByPhone(ctx, phone); err == nil && !u.PhoneConfirmedAt.Valid {
			if id, err = x.sendPhoneOTP(ctx, u, "phone_confirmation", phone); err != nil {
				return nil, err
			}
		}
	case "phone_change":
		if u, err := scanUser(x.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE phone_change=$1`, phone)); err == nil {
			if id, err = x.sendPhoneOTP(ctx, u, "phone_change", phone); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"message_id": id}, nil
}

// updatePhone handles PUT /user {phone}: a code goes to the new number,
// verifyOtp({type: 'phone_change'}) applies it.
func (x *authReq) updatePhone(ctx context.Context, u *authUser, raw string) error {
	if err := x.requirePhone(); err != nil {
		return err
	}
	phone, err := normPhone(raw)
	if err != nil {
		return err
	}
	if phone == u.Phone.String {
		return nil
	}
	if _, err := x.userByPhone(ctx, phone); err == nil {
		return authError(422, "phone_exists", "Phone number already registered by another user")
	}
	if x.cfg.Phone.Autoconfirm {
		if _, err := x.db.ExecContext(ctx, `UPDATE auth.users SET phone=$2, phone_confirmed_at=now(), updated_at=now() WHERE id=$1`, u.ID, phone); err != nil {
			return err
		}
		x.promote(ctx, u.ID, "phone")
		return nil
	}
	x.db.ExecContext(ctx, `UPDATE auth.users SET phone_change=$2 WHERE id=$1`, u.ID, phone)
	_, err = x.sendPhoneOTP(ctx, u, "phone_change", phone)
	return err
}
