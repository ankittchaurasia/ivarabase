package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"crypto/rand"

	"golang.org/x/crypto/bcrypt"
)

func b64std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// ---------------------------------------------------------------- users

type authUser struct {
	ID                 string
	Aud, Role          string
	Email              sql.NullString
	EncPW              sql.NullString
	EmailConfirmedAt   sql.NullTime
	InvitedAt          sql.NullTime
	ConfirmationSentAt sql.NullTime
	RecoverySentAt     sql.NullTime
	EmailChange        sql.NullString
	EmailChangeSentAt  sql.NullTime
	LastSignInAt       sql.NullTime
	AppMeta, UserMeta  []byte
	CreatedAt          time.Time
	UpdatedAt          time.Time
	BannedUntil        sql.NullTime
	IsAnon             bool
	Phone              sql.NullString
	PhoneConfirmedAt   sql.NullTime
	PhoneChange        sql.NullString
}

const userCols = `id, aud, role, email, encrypted_password, email_confirmed_at, invited_at, confirmation_sent_at,
	recovery_sent_at, email_change, email_change_sent_at, last_sign_in_at, raw_app_meta_data, raw_user_meta_data,
	created_at, updated_at, banned_until, is_anonymous, phone, phone_confirmed_at, phone_change`

type rowScanner interface{ Scan(...any) error }

func scanUser(r rowScanner) (*authUser, error) {
	u := &authUser{}
	err := r.Scan(&u.ID, &u.Aud, &u.Role, &u.Email, &u.EncPW, &u.EmailConfirmedAt, &u.InvitedAt, &u.ConfirmationSentAt,
		&u.RecoverySentAt, &u.EmailChange, &u.EmailChangeSentAt, &u.LastSignInAt, &u.AppMeta, &u.UserMeta,
		&u.CreatedAt, &u.UpdatedAt, &u.BannedUntil, &u.IsAnon, &u.Phone, &u.PhoneConfirmedAt, &u.PhoneChange)
	return u, err
}

func (u *authUser) banned() bool { return u.BannedUntil.Valid && u.BannedUntil.Time.After(time.Now()) }

type authSvc struct {
	app    *App
	slug   string
	secret []byte
	cfg    AuthConfig
	db     *sql.DB
}

func (a *App) authService(slug string) (*authSvc, error) {
	st, err := a.projectAPIState(slug)
	if err != nil {
		return nil, err
	}
	db, err := a.projectDB(slug)
	if err != nil {
		return nil, err
	}
	return &authSvc{app: a, slug: slug, secret: st.Secret, cfg: a.authConfig(slug), db: db}, nil
}

func (s *authSvc) userByID(ctx context.Context, id string) (*authUser, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE id=$1 AND deleted_at IS NULL`, id))
}

func (s *authSvc) userByEmail(ctx context.Context, email string) (*authUser, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE lower(email)=lower($1) AND deleted_at IS NULL`, email))
}

func (s *authSvc) identities(ctx context.Context, uid string) []map[string]any {
	out := []map[string]any{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider_id, user_id, identity_data, provider, coalesce(email,''), last_sign_in_at, created_at, updated_at
		FROM auth.identities WHERE user_id=$1 ORDER BY created_at`, uid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, pid, user, provider, email string
		var data []byte
		var last sql.NullTime
		var c, u time.Time
		rows.Scan(&id, &pid, &user, &data, &provider, &email, &last, &c, &u)
		m := map[string]any{"identity_id": id, "id": pid, "user_id": user, "identity_data": json.RawMessage(data),
			"provider": provider, "email": email, "created_at": c, "updated_at": u}
		if last.Valid {
			m["last_sign_in_at"] = last.Time
		}
		out = append(out, m)
	}
	return out
}

func (s *authSvc) userJSON(ctx context.Context, u *authUser) map[string]any {
	m := map[string]any{
		"id": u.ID, "aud": u.Aud, "role": u.Role, "email": u.Email.String, "phone": u.Phone.String,
		"app_metadata": json.RawMessage(u.AppMeta), "user_metadata": json.RawMessage(u.UserMeta),
		"identities": s.identities(ctx, u.ID), "created_at": u.CreatedAt, "updated_at": u.UpdatedAt,
		"is_anonymous": u.IsAnon,
	}
	opt := func(k string, t sql.NullTime) {
		if t.Valid {
			m[k] = t.Time
		}
	}
	opt("email_confirmed_at", u.EmailConfirmedAt)
	opt("confirmed_at", u.EmailConfirmedAt)
	opt("invited_at", u.InvitedAt)
	opt("confirmation_sent_at", u.ConfirmationSentAt)
	opt("recovery_sent_at", u.RecoverySentAt)
	opt("email_change_sent_at", u.EmailChangeSentAt)
	opt("last_sign_in_at", u.LastSignInAt)
	opt("banned_until", u.BannedUntil)
	if u.EmailChange.Valid && u.EmailChange.String != "" {
		m["new_email"] = u.EmailChange.String
	}
	opt("phone_confirmed_at", u.PhoneConfirmedAt)
	if !u.EmailConfirmedAt.Valid && u.PhoneConfirmedAt.Valid {
		m["confirmed_at"] = u.PhoneConfirmedAt.Time
	}
	if u.PhoneChange.Valid && u.PhoneChange.String != "" {
		m["new_phone"] = u.PhoneChange.String
	}
	if f := s.factorsJSON(ctx, u.ID); len(f) > 0 {
		m["factors"] = f
	}
	return m
}

func normEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || len(e) > 255 {
		return "", authError(400, "validation_failed", "Unable to validate email address: invalid format")
	}
	return e, nil
}

func (s *authSvc) checkPassword(pw string) error {
	if len(pw) < s.cfg.PasswordMinLength {
		e := authError(422, "weak_password", fmt.Sprintf("Password should be at least %d characters.", s.cfg.PasswordMinLength))
		e.Details = map[string]any{"reasons": []string{"length"}}
		return e
	}
	if len(pw) > 72 {
		return authError(422, "weak_password", "Password cannot be longer than 72 characters")
	}
	return nil
}

func hashPW(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
	return string(b), err
}

// createUser inserts a user plus its email identity.
func (s *authSvc) createUser(ctx context.Context, email, password string, userMeta map[string]any, confirmed bool, invited bool) (*authUser, error) {
	var hash any
	if password != "" {
		h, err := hashPW(password)
		if err != nil {
			return nil, err
		}
		hash = h
	}
	if userMeta == nil {
		userMeta = map[string]any{}
	}
	um, _ := json.Marshal(userMeta)
	am, _ := json.Marshal(map[string]any{"provider": "email", "providers": []string{"email"}})
	var confirmedAt, invitedAt any
	if confirmed {
		confirmedAt = time.Now()
	}
	if invited {
		invitedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `INSERT INTO auth.users(email, encrypted_password, email_confirmed_at, invited_at, raw_app_meta_data, raw_user_meta_data)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+userCols, email, hash, confirmedAt, invitedAt, am, um))
	if err != nil {
		if strings.Contains(err.Error(), "users_email_key") {
			return nil, authError(422, "email_exists", "A user with this email address has already been registered")
		}
		return nil, err
	}
	idData, _ := json.Marshal(map[string]any{"sub": u.ID, "email": email, "email_verified": confirmed, "phone_verified": false})
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth.identities(provider_id, user_id, identity_data, provider, email) VALUES ($1,$2,$3,'email',$4)`,
		u.ID, u.ID, idData, email); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// ---------------------------------------------------------------- sessions

func randToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

// accessToken signs a user JWT. methods is the session's comma-separated
// sign-in methods, newest last ("password,totp"); a TOTP step means aal2.
func (s *authSvc) accessToken(u *authUser, sessionID, methods string) (string, int64) {
	exp := time.Now().Add(time.Duration(s.cfg.JWTExpiry) * time.Second).Unix()
	aal := "aal1"
	amr := []any{}
	list := strings.Split(methods, ",")
	for i := len(list) - 1; i >= 0; i-- {
		m := strings.TrimSpace(list[i])
		if m == "" {
			continue
		}
		if m == "totp" {
			aal = "aal2"
		}
		amr = append(amr, map[string]any{"method": m, "timestamp": time.Now().Unix()})
	}
	return signJWT(s.secret, map[string]any{
		"aud": "authenticated", "exp": exp, "iat": time.Now().Unix(), "iss": s.app.projectURL(s.slug) + "/auth/v1",
		"sub": u.ID, "email": u.Email.String, "phone": u.Phone.String, "role": u.Role,
		"app_metadata": json.RawMessage(u.AppMeta), "user_metadata": json.RawMessage(u.UserMeta),
		"aal": aal, "amr": amr, "session_id": sessionID, "is_anonymous": u.IsAnon,
	}), exp
}

func (s *authSvc) issueSession(ctx context.Context, u *authUser, method, ua, ip string) (map[string]any, error) {
	if u.banned() {
		return nil, authError(400, "user_banned", "User is banned")
	}
	var sid string
	if err := s.db.QueryRowContext(ctx, `INSERT INTO auth.sessions(user_id, amr, user_agent, ip) VALUES ($1,$2,$3,$4) RETURNING id`,
		u.ID, method, trunc(ua, 300), ip).Scan(&sid); err != nil {
		return nil, err
	}
	rt := randToken(16)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth.refresh_tokens(token_hash, user_id, session_id) VALUES ($1,$2,$3)`, sha256Hex(rt), u.ID, sid); err != nil {
		return nil, err
	}
	s.db.ExecContext(ctx, `UPDATE auth.users SET last_sign_in_at=now(), updated_at=now() WHERE id=$1`, u.ID)
	if fresh, err := s.userByID(ctx, u.ID); err == nil {
		u = fresh
	}
	return s.sessionJSON(ctx, u, sid, rt, method), nil
}

func (s *authSvc) sessionJSON(ctx context.Context, u *authUser, sid, rt, method string) map[string]any {
	at, exp := s.accessToken(u, sid, method)
	return map[string]any{"access_token": at, "token_type": "bearer", "expires_in": s.cfg.JWTExpiry, "expires_at": exp,
		"refresh_token": rt, "user": s.userJSON(ctx, u)}
}

func (s *authSvc) refresh(ctx context.Context, token string) (map[string]any, error) {
	var (
		id       int64
		uid, sid string
		revoked  bool
		updated  time.Time
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, session_id, revoked, updated_at FROM auth.refresh_tokens WHERE token_hash=$1`, sha256Hex(token)).
		Scan(&id, &uid, &sid, &revoked, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, authError(400, "refresh_token_not_found", "Invalid Refresh Token: Refresh Token Not Found")
	}
	if err != nil {
		return nil, err
	}
	if revoked && time.Since(updated) > 10*time.Second {
		// reuse of an old token outside the grace window: assume theft, end the session
		s.db.ExecContext(ctx, `DELETE FROM auth.sessions WHERE id=$1`, sid)
		return nil, authError(400, "refresh_token_already_used", "Invalid Refresh Token: Already Used")
	}
	u, err := s.userByID(ctx, uid)
	if err != nil {
		return nil, authError(400, "user_not_found", "User from sub claim in JWT does not exist")
	}
	if u.banned() {
		return nil, authError(400, "user_banned", "User is banned")
	}
	var amr string
	if s.db.QueryRowContext(ctx, `SELECT amr FROM auth.sessions WHERE id=$1`, sid).Scan(&amr) != nil {
		return nil, authError(400, "session_not_found", "Invalid Refresh Token: Session Expired")
	}
	rt := randToken(16)
	s.db.ExecContext(ctx, `UPDATE auth.refresh_tokens SET revoked=true, updated_at=now() WHERE id=$1`, id)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth.refresh_tokens(token_hash, user_id, session_id, parent_hash) VALUES ($1,$2,$3,$4)`,
		sha256Hex(rt), uid, sid, sha256Hex(token)); err != nil {
		return nil, err
	}
	s.db.ExecContext(ctx, `UPDATE auth.sessions SET updated_at=now() WHERE id=$1`, sid)
	return s.sessionJSON(ctx, u, sid, rt, amr), nil
}

// ---------------------------------------------------------------- one-time tokens (links + 6-digit codes)

var linkType = map[string]string{"confirmation": "signup", "magic_link": "magiclink", "recovery": "recovery", "invite": "invite", "email_change": "email_change"}

// verifyTypes maps the `type` a client sends to the token kinds it may consume.
var verifyTypes = map[string][]string{
	"signup": {"confirmation"}, "invite": {"invite"}, "magiclink": {"magic_link"}, "recovery": {"recovery"},
	"email_change": {"email_change"}, "email": {"confirmation", "magic_link", "invite"},
}

type ott struct {
	TokenHash string // goes into the link
	OTP       string
	Link      string
}

func (s *authSvc) newToken(ctx context.Context, u *authUser, kind, relatesTo, redirect, challenge string) (*ott, error) {
	t := &ott{TokenHash: randHex(28), OTP: randOTP()}
	s.db.ExecContext(ctx, `DELETE FROM auth.one_time_tokens WHERE user_id=$1 AND token_type=$2`, u.ID, kind)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth.one_time_tokens(user_id, token_type, token_hash, otp_hash, relates_to, redirect_to, code_challenge)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, u.ID, kind, sha256Hex(t.TokenHash), sha256Hex(u.ID+":"+t.OTP), relatesTo, redirect, challenge); err != nil {
		return nil, err
	}
	q := url.Values{"token": {t.TokenHash}, "type": {linkType[kind]}}
	if redirect != "" {
		q.Set("redirect_to", redirect)
	}
	t.Link = s.app.projectURL(s.slug) + "/auth/v1/verify?" + q.Encode()
	return t, nil
}

// sendToken creates a token and emails it with the matching template.
func (s *authSvc) sendToken(ctx context.Context, u *authUser, kind, to, relatesTo, redirect, challenge string) (*ott, error) {
	t, err := s.newToken(ctx, u, kind, relatesTo, redirect, challenge)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	json.Unmarshal(u.UserMeta, &data)
	if err := s.app.sendAuthMail(ctx, s.slug, s.cfg, kind, to, mailData{ConfirmationURL: t.Link, Token: t.OTP, TokenHash: t.TokenHash,
		Email: u.Email.String, NewEmail: relatesTo, RedirectTo: redirect, Data: data}); err != nil {
		return nil, err
	}
	col := map[string]string{"confirmation": "confirmation_sent_at", "magic_link": "confirmation_sent_at", "recovery": "recovery_sent_at",
		"invite": "invited_at", "email_change": "email_change_sent_at"}[kind]
	s.db.ExecContext(ctx, `UPDATE auth.users SET `+col+`=now(), updated_at=now() WHERE id=$1`, u.ID)
	return t, nil
}

type verifiedToken struct {
	user      *authUser
	kind      string
	redirect  string
	challenge string
}

// consume validates a link token or a 6-digit code, applies its effect and deletes it.
func (s *authSvc) consume(ctx context.Context, typ, tokenHash, email, otp string) (*verifiedToken, error) {
	kinds, ok := verifyTypes[typ]
	if !ok {
		return nil, authError(400, "validation_failed", "Verify requires a verification type")
	}
	var (
		id                                  int64
		uid, kind, relates, redirect, chall string
		created                             time.Time
	)
	var err error
	switch {
	case tokenHash != "":
		err = s.db.QueryRowContext(ctx, `SELECT id, user_id, token_type, relates_to, redirect_to, code_challenge, created_at FROM auth.one_time_tokens WHERE token_hash=$1`,
			sha256Hex(tokenHash)).Scan(&id, &uid, &kind, &relates, &redirect, &chall, &created)
	case email != "" && otp != "":
		var u *authUser
		if typ == "email_change" {
			u, err = scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth.users WHERE lower(email_change)=lower($1)`, email))
		} else {
			u, err = s.userByEmail(ctx, email)
		}
		if err == nil {
			err = s.db.QueryRowContext(ctx, `SELECT id, user_id, token_type, relates_to, redirect_to, code_challenge, created_at FROM auth.one_time_tokens
				WHERE user_id=$1 AND otp_hash=$2`, u.ID, sha256Hex(u.ID+":"+strings.TrimSpace(otp))).Scan(&id, &uid, &kind, &relates, &redirect, &chall, &created)
		}
	default:
		return nil, authError(400, "validation_failed", "Verify requires either a token or a token hash")
	}
	expired := authError(403, "otp_expired", "Token has expired or is invalid")
	if err != nil || time.Since(created) > time.Hour {
		return nil, expired
	}
	match := false
	for _, k := range kinds {
		if k == kind {
			match = true
		}
	}
	if !match {
		return nil, expired
	}
	s.db.ExecContext(ctx, `DELETE FROM auth.one_time_tokens WHERE id=$1`, id)
	switch kind {
	case "email_change":
		if _, err := s.db.ExecContext(ctx, `UPDATE auth.users SET email=$2, email_change=NULL, email_confirmed_at=now(), updated_at=now() WHERE id=$1`, uid, relates); err != nil {
			return nil, err
		}
		s.db.ExecContext(ctx, `UPDATE auth.identities SET email=$2, identity_data = identity_data || jsonb_build_object('email', $2::text), updated_at=now() WHERE user_id=$1 AND provider='email'`, uid, relates)
		s.promote(ctx, uid, "email")
	default:
		s.db.ExecContext(ctx, `UPDATE auth.users SET email_confirmed_at=coalesce(email_confirmed_at, now()), updated_at=now() WHERE id=$1`, uid)
		s.db.ExecContext(ctx, `UPDATE auth.identities SET identity_data = identity_data || '{"email_verified": true}' WHERE user_id=$1 AND provider='email'`, uid)
	}
	u, err := s.userByID(ctx, uid)
	if err != nil {
		return nil, expired
	}
	return &verifiedToken{user: u, kind: kind, redirect: redirect, challenge: chall}, nil
}

// pkceChallenge stores a client's code challenge as "method:challenge".
func pkceChallenge(challenge, method string) string {
	if challenge == "" {
		return ""
	}
	m := strings.ToLower(method)
	if m == "" {
		m = "s256"
	}
	return m + ":" + challenge
}

func (s *authSvc) newFlow(ctx context.Context, uid, challenge, method, providerToken string) (string, error) {
	parts := strings.SplitN(challenge, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("bad challenge")
	}
	code := randHex(16)
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth.flow_state(auth_code, code_challenge, code_challenge_method, authentication_method, user_id, provider_token)
		VALUES ($1,$2,$3,$4,$5,$6)`, code, parts[1], parts[0], method, uid, providerToken)
	return code, err
}

func (s *authSvc) exchangePKCE(ctx context.Context, code, verifier, ua, ip string) (map[string]any, error) {
	var (
		id, chall, method, authMethod, uid, ptoken string
		created                                    time.Time
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, code_challenge, code_challenge_method, authentication_method, user_id, provider_token, created_at
		FROM auth.flow_state WHERE auth_code=$1`, code).Scan(&id, &chall, &method, &authMethod, &uid, &ptoken, &created)
	if err != nil || time.Since(created) > 5*time.Minute {
		return nil, authError(404, "flow_state_not_found", "invalid flow state, no valid flow state found")
	}
	ok := false
	switch method {
	case "s256":
		sum := sha256.Sum256([]byte(verifier))
		ok = base64.RawURLEncoding.EncodeToString(sum[:]) == chall
	case "plain":
		ok = verifier == chall
	}
	if !ok {
		return nil, authError(403, "bad_code_verifier", "code challenge does not match previously saved code verifier")
	}
	s.db.ExecContext(ctx, `DELETE FROM auth.flow_state WHERE id=$1`, id)
	u, err := s.userByID(ctx, uid)
	if err != nil {
		return nil, err
	}
	sess, err := s.issueSession(ctx, u, authMethod, ua, ip)
	if err != nil {
		return nil, err
	}
	if ptoken != "" {
		sess["provider_token"] = ptoken
	}
	return sess, nil
}

// ---------------------------------------------------------------- errors + helpers

func authError(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

// writeAuthError uses GoTrue's error shape, which supabase-js understands.
func writeAuthError(w http.ResponseWriter, err error) {
	ae := AsAPIError(err)
	body := map[string]any{"code": ae.Status, "error_code": ae.Code, "msg": ae.Message, "error": ae.Code, "error_description": ae.Message}
	if d, ok := ae.Details.(map[string]any); ok {
		if r, ok := d["reasons"]; ok {
			body["weak_password"] = map[string]any{"reasons": r}
		}
	}
	writeJSON(w, ae.Status, body)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var authLimiter = &limiter{hits: map[string][]time.Time{}}

func (l *limiter) allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var keep []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < per {
			keep = append(keep, t)
		}
	}
	if len(keep) >= n {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}

func readJSON(r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, authError(400, "bad_json", "Could not parse request body as JSON")
		}
	}
	return m, nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func obj(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return nil
}
