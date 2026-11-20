# Security Policy

## Supported Versions

| Version | Supported |
| --- | --- |
| `0.1.x` | Yes |

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report vulnerabilities privately using **GitHub Security Advisories** (**Security -> Advisories -> Report a vulnerability** on the repository).

When reporting an issue, please include:
- A clear description of the vulnerability and affected endpoints or packages
- Steps to reproduce or a proof-of-concept request
- Any suggested remediation if known

We will acknowledge reports promptly and coordinate a fix and advisory before public disclosure.

## Security Architecture Notes

MergerFin is designed with credential isolation and least-privilege execution in mind:

- **Upstream Token Encryption (`MERGERFIN_SECRET_KEY`)**: When `MERGERFIN_SECRET_KEY` is configured, upstream Jellyfin access tokens are encrypted at rest in SQLite using **AES-256-GCM** with a 32-byte key derived via **PBKDF2-SHA256** (`100,000` iterations) and the per-database `encryption_salt`. Admin API responses (`GET /admin/api/servers`) omit `AccessToken` completely.
- **Proxy Token Isolation**: Clients only authenticate with MergerFin proxy session tokens (`proxy_sessions`). Dynamic HLS `.m3u8` rewriting strips upstream `api_key` / `ApiKey` parameters from all playlist and segment URLs so upstream credentials are never leaked to clients, while injecting upstream `ApiKey` / `X-Emby-Token` server-side on proxy requests.
- **Admin Session Hardening**: `/admin/api/login` requires `IsAdmin == true`, sets an `HttpOnly`, `SameSite=Lax` session cookie (`mergerfin_admin_token`), and all `/admin/api/*` management endpoints enforce `auth.RequireAdmin`.
- **Non-Root Container**: The official Docker image (`Dockerfile`) runs as an unprivileged user (`UID 1000` / `GID 1000`) on minimal Alpine Linux with no extra runtime privileges.
