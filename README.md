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

Creating a session, whether by that `POST` or automatically on an API route, spends the `session_issue` bucket of the client IP. Reusing a token does not.

## Rate limits

Limits are token buckets. `burst` is how many requests can pass immediately. `rps` is how many tokens return each second. A rejected request returns `429` with `Retry-After`, `X-RateLimit-Limit`, and `X-RateLimit-Remaining`.

| Bucket | Key | When |
|---|---|---|
| `global` | the whole process | every limited request, after its own bucket passes |
| `default` | session | a matched route that does not set `rate_limit` |
| named policy | session | a route whose `rate_limit` names it. Several routes with the same name share one bucket per session |
| `default` | client IP | a path that matches no route, and `GET` or `DELETE` of the session endpoint |
| `session_issue` | client IP | creating a session |

`GET /healthz` and `GET /readyz` are not limited.

Buckets live in process memory. One unused for `idle_ttl` is deleted, and the next request starts from a full bucket. `max_keys` caps how many buckets are stored; past that, a new key returns `503`.

## Circuit breaker

Each upstream has one breaker, shared by every route that forwards to it.

- **Closed.** Requests are forwarded. Consecutive upstream failures are counted. A success resets the count. A `4xx` from the upstream is a success: the service answered. A client cancel and a body the gateway rejects itself are ignored.
- **Open.** After `failure_threshold` consecutive failures (`5xx`, timeout, or connection error) the breaker opens for `open_timeout`. Requests get `503` and `Retry-After`, and the upstream is not called.
- **Half-open.** When the timeout elapses, up to `half_open_max` probes are forwarded. That many successes close the breaker. Any probe failure opens it again for another `open_timeout`.

The breaker lives in process memory. A restart starts it closed.

## Operations

- `GET /healthz` — the process is up. Always `200`.
- `GET /readyz` — `200` while the gateway accepts traffic, `503` during shutdown.
- Every request has an `X-Request-ID`. A valid client value is kept; otherwise a new one is generated. It is on the response, in the logs, and on the upstream request.
- A body larger than `server.max_body_bytes` returns `413`.
- No connection to the upstream returns `502`. An upstream timeout returns `504`.
- An open circuit breaker returns `503` without calling the upstream.
- `SIGTERM` or `Ctrl+C` stops accepting connections and waits for in-flight requests up to `server.shutdown_timeout`.
- Logs are JSON on stdout (`log.level`).

## Running

Go does not have to be installed locally. Tests and the binary build run inside the `golang:1.27` Docker image.

```sh
make check         # build, vet, staticcheck, deadcode, tests
make test          # go test -race ./...
make e2e           # client, gateway, and internal service in Docker
make docker-build  # image gateway:dev
make docker-run    # :8080, config configs/gateway.yaml
```

`make e2e` builds three containers on one network. The client calls the gateway. The gateway proxies `/api/*` and `/limited/*` to the mock service. The client also calls the mock directly to switch it between success and HTTP 500 and to count how many proxied requests arrived. The run checks session issuance, the per-session rate limit, and the circuit breaker (three failures, then `503` with no further calls, then a successful probe after `open_timeout`). GitHub Actions runs the same compose file.

GitHub Actions (`.github/workflows/ci.yml`) runs the same checks on every push and pull request. The Go compiler already rejects ill-typed code. On top of that:

| Check | What it catches |
|---|---|
| `go vet` | suspicious constructs the compiler accepts, such as bad `printf` arguments or unreachable code |
| `staticcheck` | likely bugs, misuse of the standard library, deprecated APIs |
| `deadcode` | functions nothing in the program or its tests can reach |

Versions of `staticcheck` and `deadcode` are pinned in `go.mod` (`go tool ...`).

The runtime image is distroless and the process is not root. Inside the container the config is `/etc/gateway/config.yaml`.

In the example config, `upstreams.*.url` points at `host.docker.internal`, a service on the host machine.
