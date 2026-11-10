# Security

## Reporting a vulnerability

Please **don't open a public issue**. Report it privately through GitHub: **[Security → Report a vulnerability](https://github.com/ankittchaurasia/ivarabase/security/advisories/new)**. Include what is affected, how to reproduce it and what an attacker could do.

You'll get a reply within a few days. Once there's a fix, it ships in a release with a note in the changelog, and you are credited unless you'd rather not be.

## Supported versions

Fixes go into the latest release. IvaraBase is pre-1.0, so update to the newest version to get security fixes.

## How IvaraBase is meant to be deployed

- **The panel, API and MCP endpoint** are protected by the owner login and scoped tokens. Use HTTPS: built-in Let's Encrypt, or a proxy in front.
- **Tokens:** give agents the lowest level that works (`read` or `write`), limit them to projects, set an expiry, and leave `allow_destructive` off so destructive calls need your approval.
- **Docker:** publish port 8787 on `127.0.0.1` only. Docker's port rules bypass ufw and most host firewalls.
- **Postgres** listens on `127.0.0.1` unless you change it. When it accepts remote connections, it requires TLS and a password.
- **Project isolation:** each project has its own database and roles. Anon and user JWTs only reach their own project, and row level security applies to every API.
- **Recovery key:** it decrypts all backups. Keep it in a password manager, not on the server.
- **Functions** run user code with network access. Treat function code like any code you deploy.
