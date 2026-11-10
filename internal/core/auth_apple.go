package core

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Sign in with Apple, two ways:
//   - web: /authorize?provider=apple -> Apple -> POST /callback (Apple uses
//     response_mode=form_post) -> redirect_to. Needs the Services ID, Team ID,
//     Key ID and the .p8 private key.
//   - native (iOS/macOS): the app gets an id_token from AuthenticationServices
//     and calls supabase.auth.signInWithIdToken({provider: "apple", token, nonce}).
//     Needs only the app's bundle ID.
//
// Apple wants the client secret as an ES256 JWT signed with the .p8 key and
// valid for at most 6 months. IvaraBase signs a fresh one for every exchange,
// so there is no secret to regenerate.
//
// signInWithIdToken works for Google as well (One Tap, Android Credential
// Manager): the token's audience must be the Google client ID.

type AppleProvider struct {
	Enabled    bool     `json:"enabled"`
	ClientID   string   `json:"client_id"` // Services ID, e.g. com.example.web
	TeamID     string   `json:"team_id"`
	KeyID      string   `json:"key_id"`
	PrivateKey string   `json:"private_key"` // contents of AuthKey_<KEYID>.p8
	BundleIDs  []string `json:"bundle_ids"`  // native apps allowed to sign in with an id_token
}

// webReady: everything the browser redirect flow needs.
func (p AppleProvider) webReady() bool {
	return p.ClientID != "" && p.TeamID != "" && p.KeyID != "" && p.PrivateKey != ""
}

// audiences an Apple id_token may be issued to: the Services ID (web) and
// the bundle IDs (native apps).
func (p AppleProvider) audiences() []string {
	var out []string
	if p.ClientID != "" {
		out = append(out, p.ClientID)
	}
	return append(out, p.BundleIDs...)
}

// appleBase is Apple's identity server. IVARA_DEV_APPLE_URL points it at a
// local mock for testing - never set it in production.
var appleBase = "https://appleid.apple.com"

func init() {
	if u := strings.TrimRight(os.Getenv("IVARA_DEV_APPLE_URL"), "/"); u != "" {
		appleBase = u
		log.Printf("WARNING: Sign in with Apple uses %s (IVARA_DEV_APPLE_URL - testing only)", u)
	}
	oauthProviders["apple"] = oauthEndpoints{appleBase + "/auth/authorize", appleBase + "/auth/token", "name email"}
	idIssuers["apple"] = &idTokenIssuer{jwksURL: appleBase + "/auth/keys", issuers: []string{appleBase}}
}

// normalizeAppleKey accepts the .p8 file contents as pasted (with or without
// the BEGIN/END lines, or with literal \n from an .env file) and returns
// canonical PEM, or an error if it isn't an EC P-256 private key.
func normalizeAppleKey(s string) (string, *ecdsa.PrivateKey, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, `\n`, "\n"))
	if !strings.Contains(s, "-----BEGIN") {
		s = "-----BEGIN PRIVATE KEY-----\n" + s + "\n-----END PRIVATE KEY-----"
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return "", nil, fmt.Errorf("not a PEM private key - paste the whole AuthKey_XXXXXXXXXX.p8 file")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", nil, fmt.Errorf("cannot parse the private key: %v", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return "", nil, fmt.Errorf("the key must be an Apple .p8 key (EC P-256)")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: block.Bytes})), ec, nil
}

// appleClientSecret signs the short-lived ES256 client secret Apple's token
// endpoint expects.
func appleClientSecret(p AppleProvider) (string, error) {
	_, key, err := normalizeAppleKey(p.PrivateKey)
	if err != nil {
		return "", err
	}
	hdr, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": p.KeyID, "typ": "JWT"})
	now := time.Now().Unix()
	body, _ := json.Marshal(map[string]any{"iss": p.TeamID, "iat": now, "exp": now + 3600, "aud": appleBase, "sub": p.ClientID})
	unsigned := b64url.EncodeToString(hdr) + "." + b64url.EncodeToString(body)
	h := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return unsigned + "." + b64url.EncodeToString(sig), nil
}

// ---------------------------------------------------------------- id_token verification

type idTokenIssuer struct {
	jwksURL string
	issuers []string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var idIssuers = map[string]*idTokenIssuer{
	"google": {jwksURL: "https://www.googleapis.com/oauth2/v3/certs", issuers: []string{"https://accounts.google.com", "accounts.google.com"}},
}

// key returns the signing key for kid, refreshing the JWKS hourly or when an
// unknown kid shows up (at most once a minute, so bad tokens can't hammer it).
func (i *idTokenIssuer) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if k := i.keys[kid]; k != nil && time.Since(i.fetched) < time.Hour {
		return k, nil
	}
	if time.Since(i.fetched) > time.Minute || i.keys == nil {
		if keys, err := fetchJWKS(ctx, i.jwksURL); err == nil {
			i.keys, i.fetched = keys, time.Now()
		} else if i.keys == nil {
			return nil, err
		}
	}
	if k := i.keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func fetchJWKS(ctx context.Context, u string) (map[string]*rsa.PublicKey, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch signing keys: %s", resp.Status)
	}
	var set struct {
		Keys []struct {
			Kid, Kty, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("signing keys: %v", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := b64url.DecodeString(k.N)
		e, err2 := b64url.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return out, nil
}

// verifyIDToken checks an OpenID id_token's RS256 signature against the
// provider's published keys, its issuer, audience and expiry.
func verifyIDToken(ctx context.Context, provider, token string, audiences []string) (map[string]any, error) {
	iss := idIssuers[provider]
	if iss == nil {
		return nil, fmt.Errorf("provider %s does not issue id_tokens", provider)
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
	key, err := iss.key(ctx, hdr.Kid)
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
	if !contains(iss.issuers, str(c, "iss")) {
		return nil, fmt.Errorf("id_token issuer %q is not %s", str(c, "iss"), iss.issuers[0])
	}
	if !audMatches(c["aud"], audiences) {
		return nil, fmt.Errorf("id_token audience %v is not one of this project's client IDs %v", c["aud"], audiences)
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

func audMatches(aud any, allowed []string) bool {
	switch v := aud.(type) {
	case string:
		return v != "" && contains(allowed, v)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && contains(allowed, s) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkNonce follows GoTrue: the app passes the raw nonce, the id_token holds
// either its SHA-256 (hex, what Apple's docs recommend sending) or the raw value.
func checkNonce(raw string, claims map[string]any) error {
	tn := str(claims, "nonce")
	if raw == "" && tn == "" {
		return nil
	}
	if raw == "" || tn == "" {
		return fmt.Errorf("passed nonce and nonce in id_token should either both exist or not")
	}
	h := sha256.Sum256([]byte(raw))
	if tn != hex.EncodeToString(h[:]) && tn != raw {
		return fmt.Errorf("nonces mismatch")
	}
	return nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}

// idTokenProfile turns verified id_token claims into a profile.
func idTokenProfile(provider string, c map[string]any) *oauthProfileData {
	p := &oauthProfileData{ID: str(c, "sub"), Email: strings.ToLower(str(c, "email")), Verified: truthy(c["email_verified"]),
		Name: str(c, "name"), Avatar: str(c, "picture"), Raw: c}
	if provider == "apple" && truthy(c["is_private_email"]) {
		p.Verified = true // Apple relay addresses are verified by Apple
	}
	return p
}

// appleUserName reads the name Apple posts (only on the first sign-in) in the
// `user` form field: {"name":{"firstName":"..","lastName":".."},"email":".."}.
func appleUserName(raw string) string {
	var u struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &u) != nil {
		return ""
	}
	return strings.TrimSpace(u.Name.FirstName + " " + u.Name.LastName)
}

// providerIssuer is the iss recorded in identity_data.
func providerIssuer(provider string) string {
	switch provider {
	case "apple":
		return appleBase
	case "google":
		return "https://accounts.google.com"
	case "github":
		return "https://api.github.com"
	}
	return ""
}

// ---------------------------------------------------------------- signInWithIdToken

// idTokenGrant handles POST /token?grant_type=id_token
// {provider, id_token, nonce?, access_token?} from supabase-js signInWithIdToken.
func (x *authReq) idTokenGrant(ctx context.Context, body map[string]any) (any, error) {
	if !authLimiter.allow(x.slug+"|token|"+x.ip, 30, 5*time.Minute) {
		return nil, authError(429, "over_request_rate_limit", "Request rate limit reached")
	}
	provider := str(body, "provider")
	var aud []string
	switch provider {
	case "apple":
		if x.cfg.Apple.Enabled {
			aud = x.cfg.Apple.audiences()
		}
	case "google":
		if x.cfg.Google.Enabled && x.cfg.Google.ClientID != "" {
			aud = []string{x.cfg.Google.ClientID}
		}
	default:
		if o, ok := x.cfg.oidc(provider); ok {
			return x.oidcIDTokenGrant(ctx, o, body)
		}
		return nil, authError(400, "validation_failed", "Custom OIDC provider \""+provider+"\" not allowed; add it under Authentication > Providers > OpenID Connect")
	}
	if len(aud) == 0 {
		return nil, authError(400, "provider_disabled", "Provider ("+provider+") is not enabled")
	}
	tok := str(body, "id_token")
	if tok == "" {
		return nil, authError(400, "validation_failed", "id_token required")
	}
	claims, err := verifyIDToken(ctx, provider, tok, aud)
	if err != nil {
		return nil, authError(400, "bad_oauth_callback", "Bad ID token: "+err.Error())
	}
	if err := checkNonce(str(body, "nonce"), claims); err != nil {
		return nil, authError(400, "bad_oauth_callback", "Bad ID token: "+err.Error())
	}
	u, err := x.oauthUser(ctx, provider, idTokenProfile(provider, claims))
	if err != nil {
		return nil, err
	}
	authAudit(ctx, x.db, x.ip, "login", u.ID, u.Email.String, map[string]any{"provider": provider, "grant": "id_token"})
	return x.issueSession(ctx, u, "oauth", x.ua, x.ip)
}

// checkAppleConfig: enabling Apple needs either the full web setup or at
// least one bundle ID (native-only apps), and a half-filled web setup is
// almost always a mistake worth reporting.
func checkAppleConfig(p AppleProvider) error {
	web := map[string]string{"Services ID": p.ClientID, "Team ID": p.TeamID, "Key ID": p.KeyID, "private key": p.PrivateKey}
	var missing []string
	set := 0
	for _, k := range []string{"Services ID", "Team ID", "Key ID", "private key"} {
		if web[k] == "" {
			missing = append(missing, k)
		} else {
			set++
		}
	}
	if !p.Enabled {
		return nil
	}
	if set == 0 && len(p.BundleIDs) == 0 {
		return errBad("invalid_parameter", "Apple needs a Services ID, Team ID, Key ID and private key (web sign-in) or at least one bundle ID (native apps) before it can be enabled", "")
	}
	if set > 0 && set < 4 && !(set == 1 && p.ClientID != "" && len(p.BundleIDs) > 0) {
		return errBad("invalid_parameter", "Apple web sign-in is missing: "+strings.Join(missing, ", "), "set all four, or only bundle IDs for native-only sign-in")
	}
	return nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
