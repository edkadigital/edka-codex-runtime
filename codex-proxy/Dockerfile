# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/codex-proxy \
    .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/codex-proxy /usr/local/bin/codex-proxy

USER nonroot:nonroot
EXPOSE 4500
ENTRYPOINT ["/usr/local/bin/codex-proxy"]
