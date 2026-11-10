# Sign in with Apple

Two ways to use it, and a project can use both:

| | Web (browser redirect) | Native iOS / macOS |
|---|---|---|
| Client call | `signInWithOAuth({ provider: "apple" })` | `signInWithIdToken({ provider: "apple", token, nonce })` |
| Needs | Services ID, Team ID, Key ID, `.p8` key, a public HTTPS domain | only the app's bundle ID |

Apple needs a paid Apple Developer account. Everything below is in
[Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/identifiers/list).

## 1. Set it up at Apple

1. **App ID** (Identifiers → App IDs): your app's bundle ID, e.g. `com.myapp.ios`, with
   the *Sign in with Apple* capability. Native apps need nothing else.
2. **Services ID** (web only; Identifiers → Services IDs): e.g. `com.myapp.web`. Enable *Sign in
   with Apple* → Configure:
   - primary App ID: the one from step 1
   - domains: your project's host, e.g. `shop.base.example.com`
   - return URL: copy it from IvaraBase (Authentication → Sign-in methods → Apple), e.g.
     `https://shop.base.example.com/auth/v1/callback`
3. **Key** (web only; Keys → +): enable *Sign in with Apple*, download `AuthKey_XXXXXXXXXX.p8`
   (you can only download it once) and note the **Key ID**. Your **Team ID** is shown top right
   on the developer site.

Apple only redirects to HTTPS domains it knows, never `localhost` or an IP address, so the web
flow needs the server on its real domain ([DEPLOY.md](DEPLOY.md)). Native sign-in works anywhere.

## 2. Turn it on in IvaraBase

**UI:** Authentication → Sign-in methods → Apple. Paste the `.p8` file as is.

**CLI / agents:**

```sh
ivarabase auth config_set --project shop --apple_enabled true \
  --apple_client_id com.myapp.web --apple_team_id ABCDE12345 --apple_key_id XYZ9876543 \
  --apple_private_key @AuthKey_XYZ9876543.p8 --apple_bundle_ids com.myapp.ios
```

**ivarabase.yaml** ([CONFIG-AS-CODE.md](CONFIG-AS-CODE.md)):

```yaml
auth:
  apple:
    enabled: true
    client_id: com.myapp.web
    team_id: ABCDE12345
    key_id: XYZ9876543
    private_key: ${APPLE_PRIVATE_KEY}   # multi-line env var, or one line with \n in .env
    bundle_ids: [com.myapp.ios]
```

Apple's client secret is a JWT that expires after at most 6 months. IvaraBase signs a fresh one
from your `.p8` key for every sign-in, so there is nothing to regenerate. If you revoke the key at
Apple, paste the new one.

## 3. Call it from your app

**Web** (supabase-js):

```js
await supabase.auth.signInWithOAuth({ provider: "apple", options: { redirectTo: "https://myapp.com/welcome" } });
```

Apple returns the user to IvaraBase with a form POST; IvaraBase then redirects to `redirectTo`
with a session, or with `?code=` when the client uses PKCE, exactly like Google and GitHub.

**iOS** (supabase-swift):

```swift
let nonce = UUID().uuidString
let request = ASAuthorizationAppleIDProvider().createRequest()
request.requestedScopes = [.fullName, .email]
request.nonce = sha256Hex(nonce)          // Apple gets the hash...
// ... in the ASAuthorizationController delegate:
let idToken = String(data: credential.identityToken!, encoding: .utf8)!
try await supabase.auth.signInWithIdToken(
  credentials: .init(provider: .apple, idToken: idToken, nonce: nonce))   // ...IvaraBase gets the raw nonce
```

**Expo / React Native**:

```js
const raw = Crypto.randomUUID();
const hashed = await Crypto.digestStringAsync(Crypto.CryptoDigestAlgorithm.SHA256, raw);
const cred = await AppleAuthentication.signInAsync({
  requestedScopes: [AppleAuthentication.AppleAuthenticationScope.FULL_NAME, AppleAuthentication.AppleAuthenticationScope.EMAIL],
  nonce: hashed,
});
await supabase.auth.signInWithIdToken({ provider: "apple", token: cred.identityToken, nonce: raw });
```

IvaraBase checks the token's signature against Apple's published keys, the issuer, that the
audience is one of your bundle IDs or the Services ID, the expiry, and the nonce. A user who signs
in on the web and in the app gets the same account (same Apple user ID).

## Good to know

- **Names arrive once.** Apple sends the user's name only on their first sign-in. The web flow
  saves it to `user_metadata.full_name`; native apps get it in the credential and should save it
  with `supabase.auth.updateUser({ data: { full_name } })`.
- **Hidden emails.** Users can choose an `@privaterelay.appleid.com` address. To deliver auth
  emails to those addresses, register your sending domain and from-address in Apple's
  *Sign in with Apple for Email Communication* settings.
- **Google works the same way natively:** `signInWithIdToken({ provider: "google", token })`
  accepts Google id_tokens (One Tap, Android Credential Manager) whose audience is the Google
  client ID configured for the project.
- **Testing without Apple:** `make test-apple` starts a throwaway instance wired to a mock of
  Apple's servers and runs 38 checks: web, PKCE, native, rejected tokens, and config as code.
