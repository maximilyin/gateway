# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockservice ./test/mockservice

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mockservice /mockservice
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/mockservice"]
