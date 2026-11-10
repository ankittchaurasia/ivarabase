package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Minimal HS256 JWTs: the project API keys (anon / service_role) and, later,
// end-user session tokens. Same format Supabase and PostgREST use.

var b64url = base64.RawURLEncoding

func signJWT(secret []byte, claims map[string]any) string {
	header := b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	unsigned := header + "." + b64url.EncodeToString(body)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(unsigned))
	return unsigned + "." + b64url.EncodeToString(m.Sum(nil))
}

var errBadJWT = errors.New("invalid JWT")

// verifyJWT checks the signature and expiry and returns the claims.
func verifyJWT(secret []byte, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errBadJWT
	}
	hb, err := b64url.DecodeString(parts[0])
	if err != nil {
		return nil, errBadJWT
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256" {
		return nil, errBadJWT
	}
	sig, err := b64url.DecodeString(parts[2])
	if err != nil {
		return nil, errBadJWT
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(sig, m.Sum(nil)) != 1 {
		return nil, errBadJWT
	}
	body, err := b64url.DecodeString(parts[1])
	if err != nil {
		return nil, errBadJWT
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, errBadJWT
	}
	if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp) {
		return nil, errors.New("JWT expired")
	}
	return claims, nil
}
