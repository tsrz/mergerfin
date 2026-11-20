# Contributing to MergerFin

Thank you for your interest in contributing to **MergerFin**!

## Prerequisites

- **Go 1.27+**
- **GCC / CGO** (optional, required for running tests with `-race`)
- **golangci-lint v2.15+**
- **Docker & Docker Compose v2** (for multi-upstream local integration testing)

## Local Development

### 1. Run Unit Tests & Linter

```bash
# Build static binary into bin/mergerfin
make build

# Run full test suite (uses -race when gcc is available)
make test

# Run golangci-lint
make lint
```

### 2. Multi-Upstream Docker Dev Stack

`docker-compose.dev.yml` launches MergerFin alongside two isolated Jellyfin `12.4` upstream containers (`jellyfin-upstream-a` and `jellyfin-upstream-b`):

| Service | Default Host Port | Purpose |
| --- | --- | --- |
| `mergerfin` | `18096` | MergerFin proxy (`/web` and `/admin`) |
| `jellyfin-upstream-a` | `18097` | Upstream Jellyfin instance A |
| `jellyfin-upstream-b` | `18098` | Upstream Jellyfin instance B |

Start the stack and seed test upstreams:

```bash
make dev-up
make dev-seed
```

To tear down the dev stack:

```bash
make dev-down
```

## Commit Style

Please follow [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles:

- `feat:` — New features or Jellyfin API capabilities
- `fix:` — Bug fixes and compatibility adjustments
- `test:` — Adding or improving unit/integration tests
- `ci:` — CI/CD workflow, Docker, or linter changes
- `docs:` — Documentation updates (`README.md`, `CONTRIBUTING.md`, etc.)

## Pull Request Checklist

- [ ] Code compiles cleanly with `CGO_ENABLED=0 go build ./cmd/mergerfin`
- [ ] All tests pass with `CGO_ENABLED=1 go test -v -race ./...`
- [ ] `golangci-lint run` passes with zero warnings
- [ ] New Jellyfin endpoints enforce 32-char lowercase hex IDs and non-null slices/maps
- [ ] No upstream credentials (`AccessToken` / `ApiKey`) are exposed in downstream responses or playlists
- [ ] Documentation updated where configuration or behavior changed
