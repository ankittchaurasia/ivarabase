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
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testP8(t *testing.T) (string, *ecdsa.PrivateKey) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), k
}

func TestAppleKeyAndClientSecret(t *testing.T) {
	p8, key := testP8(t)
	// pasted variants all normalize to the same PEM
	body := strings.Join(strings.Split(strings.TrimSpace(p8), "\n")[1:len(strings.Split(strings.TrimSpace(p8), "\n"))-1], "\n")
	for _, in := range []string{p8, "  " + p8 + "\n\n", strings.ReplaceAll(p8, "\n", `\n`), body} {
		norm, _, err := normalizeAppleKey(in)
		if err != nil || norm != p8 {
			t.Fatalf("normalize %q: %v", in[:20], err)
		}
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rder, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	if _, _, err := normalizeAppleKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rder}))); err == nil {
		t.Fatal("RSA key accepted")
	}
	if _, _, err := normalizeAppleKey("not a key"); err == nil {
		t.Fatal("garbage accepted")
	}

	sec, err := appleClientSecret(AppleProvider{ClientID: "com.example.web", TeamID: "TEAM123456", KeyID: "KEY1234567", PrivateKey: p8})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(sec, ".")
	var hdr, claims map[string]any
	hb, _ := b64url.DecodeString(parts[0])
	cb, _ := b64url.DecodeString(parts[1])
	json.Unmarshal(hb, &hdr)
	json.Unmarshal(cb, &claims)
	if hdr["alg"] != "ES256" || hdr["kid"] != "KEY1234567" || claims["iss"] != "TEAM123456" || claims["sub"] != "com.example.web" || claims["aud"] != appleBase {
		t.Fatalf("bad secret: %v %v", hdr, claims)
	}
	sig, _ := b64url.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("client secret signature does not verify")
	}
}

func TestVerifyIDToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": b64url.EncodeToString(key.N.Bytes()), "e": b64url.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer srv.Close()
	idIssuers["test"] = &idTokenIssuer{jwksURL: srv.URL, issuers: []string{"https://issuer.test"}}
	defer delete(idIssuers, "test")

	mint := func(kid string, c map[string]any) string {
		hb, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid})
		cb, _ := json.Marshal(c)
		u := b64url.EncodeToString(hb) + "." + b64url.EncodeToString(cb)
		h := sha256.Sum256([]byte(u))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		return u + "." + b64url.EncodeToString(sig)
	}
	now := time.Now().Unix()
	good := map[string]any{"iss": "https://issuer.test", "aud": "com.example.ios", "sub": "001.abc", "exp": now + 600, "iat": now,
		"email": "X@privaterelay.appleid.com", "email_verified": "true", "is_private_email": "true"}
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for a, b := range good {
			c[a] = b
		}
		c[k] = v
		return c
	}
	ctx := context.Background()
	aud := []string{"com.example.web", "com.example.ios"}
	c, err := verifyIDToken(ctx, "test", mint("k1", good), aud)
	if err != nil {
		t.Fatal(err)
	}
	if p := idTokenProfile("apple", c); p.ID != "001.abc" || !p.Verified || p.Email != "x@privaterelay.appleid.com" {
		t.Fatalf("profile %+v", p)
	}
	if _, err := verifyIDToken(ctx, "test", mint("k1", with("aud", []any{"other", "com.example.web"})), aud); err != nil {
		t.Fatalf("aud list: %v", err)
	}
	bad := map[string]string{
		"wrong audience": mint("k1", with("aud", "com.evil.app")),
		"wrong issuer":   mint("k1", with("iss", "https://evil.test")),
		"expired":        mint("k1", with("exp", now-3600)),
		"unknown key":    mint("k2", good),
		"tampered":       strings.Replace(mint("k1", good), ".", ".e", 1),
	}
	for name, tok := range bad {
		if _, err := verifyIDToken(ctx, "test", tok, aud); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if fetches > 2 {
		t.Errorf("JWKS fetched %d times; unknown kids must not refetch every call", fetches)
	}
	// HS256 token signed with a guessable secret must be refused outright
	if _, err := verifyIDToken(ctx, "test", signJWT([]byte("x"), good), aud); err == nil {
		t.Error("HS256 accepted")
	}

	raw := "n0nce"
	h := sha256.Sum256([]byte(raw))
	for _, tc := range []struct {
		raw, inToken string
		ok           bool
	}{{"", "", true}, {raw, hex.EncodeToString(h[:]), true}, {raw, raw, true}, {raw, "", false}, {"", "abc", false}, {"other", hex.EncodeToString(h[:]), false}} {
		c := map[string]any{}
		if tc.inToken != "" {
			c["nonce"] = tc.inToken
		}
		if err := checkNonce(tc.raw, c); (err == nil) != tc.ok {
			t.Errorf("nonce %q vs %q: %v", tc.raw, tc.inToken, err)
		}
	}
	if appleUserName(`{"name":{"firstName":"Asha","lastName":"Rao"},"email":"a@b.c"}`) != "Asha Rao" || appleUserName("") != "" {
		t.Error("apple user name")
	}
}

func TestCheckAppleConfig(t *testing.T) {
	ok := []AppleProvider{
		{}, // disabled
		{Enabled: true, BundleIDs: []string{"a.b"}}, // native only
		{Enabled: true, ClientID: "web", BundleIDs: []string{"a.b"}},
		{Enabled: true, ClientID: "web", TeamID: "T", KeyID: "K", PrivateKey: "P"},
	}
	for _, p := range ok {
		if err := checkAppleConfig(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, p := range []AppleProvider{{Enabled: true}, {Enabled: true, ClientID: "web", TeamID: "T"}, {Enabled: true, ClientID: "web"}} {
		if checkAppleConfig(p) == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}
