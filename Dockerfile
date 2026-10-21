# build
FROM golang:1.26-alpine AS builder
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION shows up in /health, so a deploy can be told apart from the last one.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/orderly/orderly-backend/internal/httpserver.Version=${VERSION}" \
    -o /out/server ./cmd/server

# runtime
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/server /server
EXPOSE 8080
USER nonroot:nonroot
# No shell in this image, so the binary probes itself (see cmd/server/healthcheck.go).
# start-period leaves room for the first database connection.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/server", "healthcheck"]
ENTRYPOINT ["/server"]
