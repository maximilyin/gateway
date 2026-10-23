# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/e2e ./test/e2e

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/e2e /e2e
USER nonroot:nonroot
ENTRYPOINT ["/e2e"]
