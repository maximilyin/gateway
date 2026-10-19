# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src

# Download modules in a separate layer so code changes do not invalidate the cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 produces a fully static binary that runs on an image without libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# distroless/static has no shell or package manager: only CA certs, tzdata and a
# non-root user, which keeps the attack surface minimal.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
COPY configs/gateway.yaml /etc/gateway/config.yaml

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/gateway"]
CMD ["-config", "/etc/gateway/config.yaml"]
