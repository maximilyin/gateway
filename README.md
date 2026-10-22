# gateway

HTTP gateway in Go. It accepts external requests and proxies them to internal services according to a YAML config.

Example config: [`configs/gateway.yaml`](configs/gateway.yaml). Run: `gateway -config configs/gateway.yaml`.

## Routing

`upstreams` names the internal services. `routes` decides which requests go where.

- `/service/users/me` matches only that path.
- `/service/users/*` matches `/service/users` and everything below it. `/service/usersX` does not match.
- The most specific rule wins: an exact path first, then the longest prefix. Declaration order does not matter.
- `methods` restricts methods. `GET` also allows `HEAD`. A path that matches with a disallowed method returns `405` and an `Allow` header. An unknown path returns `404`.
- `strip_prefix` is removed before the request is forwarded: `/service/users/42` with prefix `/service/users` becomes `/42`.
- A path such as `/public/../admin` is not routed. The client is redirected to the cleaned path.

Routes that share an upstream share one connection pool. An upstream timeout must be shorter than `server.write_timeout`.

## Sessions

The client does not call a separate endpoint before its first request.

1. The client calls any configured route without a token.
2. The gateway creates a session, proxies the request, and returns the token in the `X-Session-Token` response header (the name is `sessions.header`).
3. The client sends that header on later requests and stays on the same session. The response does not carry a new token.
4. An expired, unknown, or revoked token on an API route is replaced with a new session, and that same request is proxied with it.

A token is 32 random bytes. Only its SHA-256 is stored, and that hash is forwarded upstream as `X-Session-ID`. The raw token and any client-supplied `X-Session-ID` are removed before the upstream call.

A `404` does not create a session. Issuance happens only after a route has matched.

| | |
|---|---|
| `ttl` | How long an idle session lives. Use extends it once less than half of the TTL remains |
| `max_lifetime` | Hard cap from the moment the session was created |
| `max_sessions` | Memory ceiling. Issuing past it returns `503` |
| `cleanup_interval` | How often expired sessions are deleted |

Sessions live in the process memory only. A restart drops them, and replicas do not share them.

Explicit operations on `sessions.endpoint` (default `/auth/session`):

| Method | Result |
|---|---|
| `POST` | Create a session. `201` and JSON with `token`, `session_id`, `expires_at` |
| `GET` | Describe the session for the token. A dead token returns `401` |
| `DELETE` | Revoke the session. `204`. A dead token returns `401` |

`GET` and `DELETE` do not issue a replacement. The next API request that still carries the old token receives a new session.

## Operations

- `GET /healthz` — the process is up. Always `200`.
- `GET /readyz` — `200` while the gateway accepts traffic, `503` during shutdown.
- Every request has an `X-Request-ID`. A valid client value is kept; otherwise a new one is generated. It is on the response, in the logs, and on the upstream request.
- A body larger than `server.max_body_bytes` returns `413`.
- No connection to the upstream returns `502`. An upstream timeout returns `504`.
- `SIGTERM` or `Ctrl+C` stops accepting connections and waits for in-flight requests up to `server.shutdown_timeout`.
- Logs are JSON on stdout (`log.level`).

## Running

Go does not have to be installed locally. Tests and the binary build run inside the `golang:1.27` Docker image.

```sh
make check         # build, vet, staticcheck, deadcode, tests
make test          # go test -race ./...
make docker-build  # image gateway:dev
make docker-run    # :8080, config configs/gateway.yaml
```

GitHub Actions (`.github/workflows/ci.yml`) runs the same checks on every push and pull request. The Go compiler already rejects ill-typed code. On top of that:

| Check | What it catches |
|---|---|
| `go vet` | suspicious constructs the compiler accepts, such as bad `printf` arguments or unreachable code |
| `staticcheck` | likely bugs, misuse of the standard library, deprecated APIs |
| `deadcode` | functions nothing in the program or its tests can reach |

Versions of `staticcheck` and `deadcode` are pinned in `go.mod` (`go tool ...`).

The runtime image is distroless and the process is not root. Inside the container the config is `/etc/gateway/config.yaml`.

In the example config, `upstreams.*.url` points at `host.docker.internal`, a service on the host machine.

## Not implemented yet

Rate limits and the circuit breaker are planned. This code does not have them.
