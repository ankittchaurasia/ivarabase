# Auth

Each project has its own users, in its own `auth` schema, at `/auth/v1` (GoTrue-compatible). Use `supabase.auth.*`. Configure it on the project's **Authentication** page, or with `auth.config_set` from an agent or the CLI.

Access tokens are JWTs signed with the project's secret, with `role: authenticated`, `sub` (the user id), `email`, `phone`, `aal`, `amr`, `is_anonymous`, `app_metadata` and `user_metadata`. RLS reads them through `auth.uid()` and `auth.jwt()`.

## Email

On by default: email + password, magic links, 6-digit email codes, password recovery, email change, invites.

- **Require email confirmation** before password sign-in, or not (off by default).
- **SMTP** for real delivery. Without it, every email lands in the project's **Mailbox** tab with working links and codes, which is handy during development.
- **Templates** for each email, using Supabase's variables: `{{ .ConfirmationURL }}`, `{{ .Token }}`, `{{ .SiteURL }}`, `{{ .Email }}`.
- **URL configuration:** the site URL (default redirect) and the allowed redirect URLs. A trailing `*` matches anything after it.

## Phone (SMS codes)

```ts
await supabase.auth.signInWithOtp({ phone: "+15551234567" });
await supabase.auth.verifyOtp({ phone: "+15551234567", token: "123456", type: "sms" });
// or phone + password
await supabase.auth.signUp({ phone: "+15551234567", password });
await supabase.auth.signInWithPassword({ phone: "+15551234567", password });
// change number: a code goes to the new one
await supabase.auth.updateUser({ phone: "+15557654321" });
await supabase.auth.verifyOtp({ phone: "+15557654321", token, type: "phone_change" });
```

Turn it on under **Authentication → Sign-in methods → Phone** and pick how codes are sent:

- **Twilio:** account SID, auth token, and a sender number or Messaging Service SID.
- **Webhook,** for any other provider (MessageBird, Vonage, AWS SNS, a local gateway...). IvaraBase POSTs `{"type":"sms","kind":"otp","project":"shop","phone":"+15551234567","otp":"123456","message":"Your code is 123456"}` to your URL, and your endpoint sends the SMS. With a signing secret, requests carry `X-Ivarabase-Timestamp` and `X-Ivarabase-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">`.
- **None:** nothing is sent, and codes appear in the **SMS log** tab (development).

Codes last 5 minutes (configurable from 60 to 3600 seconds). A number gets at most one code a minute and five an hour, and ten wrong attempts lock verification for 15 minutes. **Test numbers** get a fixed code and nothing is sent, which is useful for app store review and automated tests. Phones are stored as digits without `+`, like Supabase.

## Anonymous users

```ts
const { data } = await supabase.auth.signInAnonymously();
// later, keep their data and make them permanent:
await supabase.auth.updateUser({ email: "me@example.com" });
```

Turn it on under **Sign-in methods → Anonymous sign-ins**. Anonymous users are real users with `role: authenticated` and `is_anonymous: true` in the JWT, so RLS on `auth.uid()` works for them. When they add an email (once confirmed, if confirmation is on) or a phone, they become permanent and keep the same id and rows. To tell them apart in a policy:

```sql
create policy "only permanent users can post" on posts for insert to authenticated
  with check ((auth.jwt()->>'is_anonymous')::boolean is not true);
```

Sign-ups are limited to 30 an hour per IP address.

## Multi-factor authentication (TOTP)

Authenticator apps (Google Authenticator, 1Password, Authy...) through `supabase.auth.mfa`. On by default.

```ts
const { data: factor } = await supabase.auth.mfa.enroll({ factorType: "totp", friendlyName: "phone" });
// show factor.totp.qr_code (an SVG data URI) or factor.totp.secret, then:
await supabase.auth.mfa.challengeAndVerify({ factorId: factor.id, code: "123456" });

// on later sign-ins
const { data: aal } = await supabase.auth.mfa.getAuthenticatorAssuranceLevel();
if (aal.nextLevel === "aal2" && aal.currentLevel !== "aal2") {
  const { data: { totp } } = await supabase.auth.mfa.listFactors();
  await supabase.auth.mfa.challengeAndVerify({ factorId: totp[0].id, code });
}
```

A verified code upgrades the session to `aal2`, and refreshed tokens keep it. Require it in RLS:

```sql
create policy "aal2 only" on invoices for select to authenticated
  using ((auth.jwt()->>'aal') = 'aal2' and user_id = auth.uid());
```

- Each code works once, within ±30 seconds. Verification is limited to 10 tries per 5 minutes per factor.
- Enrolling another factor, or removing a verified one, needs an `aal2` session.
- Secrets are encrypted with the server's master key.
- A user who lost their device: **Users → (user) → Reset MFA** removes their factors and signs them out (`auth.user_mfa_reset`). The admin API (`auth.admin.mfa.listFactors` / `deleteFactor`) works too.
- The issuer shown in the app defaults to the site URL's host. Change it under **Multi-factor**.

## Google, GitHub, Apple

Paste the client ID and secret under **Sign-in methods**, and register the callback URL shown there with the provider. Apps call `supabase.auth.signInWithOAuth({ provider: "google" })`. Native apps can use `signInWithIdToken` for Apple and Google. Apple has its own guide: [APPLE.md](APPLE.md).

## OpenID Connect (Keycloak, Auth0, Okta, Entra, ...)

Any OIDC provider with discovery works: Keycloak, Auth0, Okta, Microsoft Entra ID (Azure AD), Authentik, Zitadel, GitLab, Google Workspace, and others.

1. Create an OAuth/OIDC client at the provider with the **callback URL** from **Sign-in methods → OpenID Connect providers**, for example `https://shop.base.example.com/auth/v1/callback`.
2. **Add provider:** a name (what apps pass as `provider`), the issuer URL (the part before `/.well-known/openid-configuration`, such as `https://sso.example.com/realms/main`), the client ID and the secret.

```ts
await supabase.auth.signInWithOAuth({ provider: "keycloak", options: { redirectTo: "https://app.example.com/welcome" } });
// native apps holding an id_token from the provider:
await supabase.auth.signInWithIdToken({ provider: "keycloak", token: idToken });
```

ID tokens must be signed with RS256 (the OIDC default). Each sign-in checks the signature, the issuer, the audience (your client ID), expiry and the nonce. When a provider returns a verified email that matches an existing user, the identity is linked to that user. Otherwise a new user is created, unless sign-ups are off.

## Users

The **Users** tab lists users by email, phone or id. From there you can create and invite users, confirm emails, send magic links and recovery emails, ban, sign out everywhere, reset MFA and delete. Agents use the `auth.user_*` operations, and apps use the service_role key with `supabase.auth.admin.*`. **Logs** shows sign-ups, sign-ins, failures and MFA events.

## Limits

| | Default |
|---|---|
| Password sign-in attempts | 30 per IP per 5 minutes |
| Sign-ups | 30 per IP per hour |
| Emails | 30 per hour per project (configurable) |
| Recovery emails | 3 per address per hour |
| SMS codes | 1 per minute and 5 per hour per number |
| Code verification | 10 tries per address/number per 15 minutes |
| Access token lifetime | 1 hour (configurable from 5 minutes to 7 days) |
