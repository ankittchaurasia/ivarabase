package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Multi-factor authentication with TOTP authenticator apps, matching
// supabase-js `auth.mfa.*` (GoTrue's /factors API):
//   POST   /factors                      enroll: secret + otpauth URI + QR SVG
//   POST   /factors/{id}/challenge       start a challenge (5 minutes)
//   POST   /factors/{id}/verify          check a code: verifies the factor and
//                                        upgrades the session to aal2
//   DELETE /factors/{id}                 unenroll (needs aal2 for a verified factor)
// A session's sign-in methods are kept in auth.sessions.amr; "totp" in that
// list makes access tokens carry aal2. Secrets are sealed with the server's
// master key, codes can't be replayed (last used time step per factor).

const totpStep = 30

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	h := hmac.New(sha1.New, secret)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// totpMatch returns the time step a code belongs to (now ±1 step), or 0.
func totpMatch(secret []byte, code string, now time.Time) int64 {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0
	}
	cur := now.Unix() / totpStep
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			return s
		}
	}
	return 0
}

func (s *authSvc) factorsJSON(ctx context.Context, uid string) []map[string]any {
	out := []map[string]any{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, friendly_name, factor_type, status, created_at, updated_at FROM auth.mfa_factors WHERE user_id=$1 ORDER BY created_at`, uid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, typ, status string
		var c, u time.Time
		if rows.Scan(&id, &name, &typ, &status, &c, &u) == nil {
			out = append(out, map[string]any{"id": id, "friendly_name": name, "factor_type": typ, "status": status, "created_at": c, "updated_at": u})
		}
	}
	return out
}

func (s *authSvc) hasVerifiedFactor(ctx context.Context, uid string) bool {
	var ok bool
	s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth.mfa_factors WHERE user_id=$1 AND status='verified')`, uid).Scan(&ok)
	return ok
}

func (s *authSvc) mfaIssuer() string {
	if s.cfg.MFA.Issuer != "" {
		return s.cfg.MFA.Issuer
	}
	if u, err := url.Parse(s.cfg.SiteURL); err == nil && u.Hostname() != "" && u.Hostname() != "localhost" {
		return u.Hostname()
	}
	return s.slug
}

// serveFactors handles /factors and /factors/{id}[/challenge|/verify].
func (x *authReq) serveFactors(ctx context.Context, sub string) {
	if x.cfg.MFA.DisableTOTP {
		writeAuthError(x.w, authError(422, "mfa_totp_enroll_not_enabled", "MFA enroll is disabled for TOTP"))
		return
	}
	u, claims, err := x.requireUser(ctx)
	if err != nil {
		writeAuthError(x.w, err)
		return
	}
	aal, _ := claims["aal"].(string)
	sid, _ := claims["session_id"].(string)
	parts := strings.Split(strings.Trim(strings.TrimPrefix(sub, "/factors"), "/"), "/")
	m := x.r.Method
	switch {
	case parts[0] == "" && m == "POST":
		x.respond(x.enrollFactor(ctx, u, aal))
	case len(parts) == 2 && parts[1] == "challenge" && m == "POST":
		x.respond(x.challengeFactor(ctx, u, parts[0]))
	case len(parts) == 2 && parts[1] == "verify" && m == "POST":
		x.respond(x.verifyFactor(ctx, u, sid, parts[0]))
	case len(parts) == 1 && parts[0] != "" && m == "DELETE":
		x.respond(x.unenrollFactor(ctx, u, aal, parts[0]))
	default:
		writeAuthError(x.w, authError(404, "not_found", "unknown factors endpoint "+m+" "+sub))
	}
}

func (x *authReq) enrollFactor(ctx context.Context, u *authUser, aal string) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	switch str(body, "factor_type") {
	case "totp":
	case "phone":
		return nil, authError(422, "mfa_phone_enroll_not_enabled", "MFA enroll is disabled for Phone")
	default:
		return nil, authError(400, "validation_failed", "factor_type needs to be totp")
	}
	if u.IsAnon {
		return nil, authError(422, "validation_failed", "anonymous users cannot enroll MFA factors")
	}
	if aal != "aal2" && x.hasVerifiedFactor(ctx, u.ID) {
		return nil, authError(422, "insufficient_aal", "AAL2 required to enroll a new factor")
	}
	// unfinished enrollments older than a day are dropped, like GoTrue
	x.db.ExecContext(ctx, `DELETE FROM auth.mfa_factors WHERE user_id=$1 AND status='unverified' AND created_at < now() - interval '1 day'`, u.ID)
	var n int
	x.db.QueryRowContext(ctx, `SELECT count(*) FROM auth.mfa_factors WHERE user_id=$1`, u.ID).Scan(&n)
	if n >= x.cfg.MFA.MaxFactors {
		return nil, authError(422, "too_many_enrolled_mfa_factors", "Maximum number of enrolled factors reached, unenroll to continue")
	}
	name := strings.TrimSpace(str(body, "friendly_name"))
	if name != "" {
		var dup bool
		x.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth.mfa_factors WHERE user_id=$1 AND friendly_name=$2)`, u.ID, name).Scan(&dup)
		if dup {
			return nil, authError(422, "mfa_factor_name_conflict", fmt.Sprintf("A factor with the friendly name %q for this user already exists", name))
		}
	}
	issuer := strings.TrimSpace(str(body, "issuer"))
	if issuer == "" {
		issuer = x.mfaIssuer()
	}
	secret := make([]byte, 20)
	rand.Read(secret)
	enc := totpB32.EncodeToString(secret)
	account := u.Email.String
	if account == "" {
		account = u.Phone.String
	}
	uri := fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s", url.PathEscape(issuer), url.PathEscape(account), enc, url.QueryEscape(issuer))
	svg, err := qrSVG(uri)
	if err != nil {
		return nil, err
	}
	var id string
	if err := x.db.QueryRowContext(ctx, `INSERT INTO auth.mfa_factors(user_id, friendly_name, factor_type, secret) VALUES ($1,$2,'totp',$3) RETURNING id`,
		u.ID, name, seal(x.app.masterKey, enc)).Scan(&id); err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "factor_enrolled", u.ID, u.Email.String, map[string]any{"factor_id": id})
	return map[string]any{"id": id, "type": "totp", "friendly_name": name, "totp": map[string]any{"qr_code": svg, "secret": enc, "uri": uri}}, nil
}

func (x *authReq) factor(ctx context.Context, u *authUser, id string) (status string, secret []byte, lastStep int64, err error) {
	var enc string
	if !uuidLike(id) {
		return "", nil, 0, authError(404, "mfa_factor_not_found", "Factor not found")
	}
	err = x.db.QueryRowContext(ctx, `SELECT status, secret, last_used_step FROM auth.mfa_factors WHERE id=$1 AND user_id=$2`, id, u.ID).Scan(&status, &enc, &lastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, 0, authError(404, "mfa_factor_not_found", "Factor not found")
	}
	if err != nil {
		return "", nil, 0, err
	}
	raw, err := unseal(x.app.masterKey, enc)
	if err != nil {
		return "", nil, 0, err
	}
	secret, err = totpB32.DecodeString(raw)
	return status, secret, lastStep, err
}

func (x *authReq) challengeFactor(ctx context.Context, u *authUser, fid string) (any, error) {
	if _, _, _, err := x.factor(ctx, u, fid); err != nil {
		return nil, err
	}
	if !authLimiter.allow(x.slug+"|mfa-challenge|"+fid, 30, 5*time.Minute) {
		return nil, authError(429, "over_request_rate_limit", "Request rate limit reached")
	}
	var id string
	var at time.Time
	if err := x.db.QueryRowContext(ctx, `INSERT INTO auth.mfa_challenges(factor_id, ip_address) VALUES ($1,$2) RETURNING id, created_at`, fid, x.ip).Scan(&id, &at); err != nil {
		return nil, err
	}
	x.db.ExecContext(ctx, `UPDATE auth.mfa_factors SET last_challenged_at=now() WHERE id=$1`, fid)
	x.db.ExecContext(ctx, `DELETE FROM auth.mfa_challenges WHERE created_at < now() - interval '1 day'`)
	return map[string]any{"id": id, "type": "totp", "expires_at": at.Add(5 * time.Minute).Unix()}, nil
}

func (x *authReq) verifyFactor(ctx context.Context, u *authUser, sid, fid string) (any, error) {
	body, err := readJSON(x.r)
	if err != nil {
		return nil, err
	}
	status, secret, lastStep, err := x.factor(ctx, u, fid)
	if err != nil {
		return nil, err
	}
	if !authLimiter.allow(x.slug+"|mfa-verify|"+fid, 10, 5*time.Minute) {
		return nil, authError(429, "over_request_rate_limit", "Too many verification attempts, try again in a few minutes")
	}
	cid := str(body, "challenge_id")
	var created time.Time
	var verified sql.NullTime
	if !uuidLike(cid) || x.db.QueryRowContext(ctx, `SELECT created_at, verified_at FROM auth.mfa_challenges WHERE id=$1 AND factor_id=$2`, cid, fid).Scan(&created, &verified) != nil {
		return nil, authError(404, "mfa_challenge_not_found", "Challenge not found")
	}
	if verified.Valid || time.Since(created) > 5*time.Minute {
		return nil, authError(422, "mfa_challenge_expired", "Challenge has expired, create a new one")
	}
	step := totpMatch(secret, str(body, "code"), time.Now())
	if step == 0 || step <= lastStep {
		authAudit(ctx, x.db, x.ip, "mfa_verify_failed", u.ID, u.Email.String, map[string]any{"factor_id": fid})
		return nil, authError(422, "mfa_verification_failed", "Invalid TOTP code entered")
	}
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// the step check inside the UPDATE makes two racing requests with one code fail one of them
	res, err := tx.ExecContext(ctx, `UPDATE auth.mfa_factors SET last_used_step=$2, status='verified', updated_at=now() WHERE id=$1 AND last_used_step < $2`, fid, step)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, authError(422, "mfa_verification_failed", "Invalid TOTP code entered")
	}
	tx.ExecContext(ctx, `UPDATE auth.mfa_challenges SET verified_at=now() WHERE id=$1`, cid)
	var amr string
	if err := tx.QueryRowContext(ctx, `SELECT amr FROM auth.sessions WHERE id=$1 AND user_id=$2`, sid, u.ID).Scan(&amr); err != nil {
		return nil, authError(403, "session_not_found", "Session from session_id claim in JWT does not exist")
	}
	if !contains(strings.Split(amr, ","), "totp") {
		amr += ",totp"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth.sessions SET aal='aal2', amr=$2, updated_at=now() WHERE id=$1`, sid, amr); err != nil {
		return nil, err
	}
	rt := randToken(16)
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth.refresh_tokens(token_hash, user_id, session_id) VALUES ($1,$2,$3)`, sha256Hex(rt), u.ID, sid); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if status != "verified" {
		authAudit(ctx, x.db, x.ip, "factor_verified", u.ID, u.Email.String, map[string]any{"factor_id": fid})
	}
	authAudit(ctx, x.db, x.ip, "mfa_verified", u.ID, u.Email.String, map[string]any{"factor_id": fid})
	fresh, err := x.userByID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return x.sessionJSON(ctx, fresh, sid, rt, amr), nil
}

func (x *authReq) unenrollFactor(ctx context.Context, u *authUser, aal, fid string) (any, error) {
	status, _, _, err := x.factor(ctx, u, fid)
	if err != nil {
		return nil, err
	}
	if status == "verified" && aal != "aal2" {
		return nil, authError(422, "insufficient_aal", "AAL2 required to unenroll verified factor")
	}
	if _, err := x.db.ExecContext(ctx, `DELETE FROM auth.mfa_factors WHERE id=$1 AND user_id=$2`, fid, u.ID); err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "factor_unenrolled", u.ID, u.Email.String, map[string]any{"factor_id": fid})
	return map[string]any{"id": fid}, nil
}
