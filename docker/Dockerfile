FROM golang:1.26.6-alpine AS builder

WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/go-llm-proxy .

FROM alpine:3.21

RUN addgroup -S app && adduser -S -G app app \
    && apk add --no-cache ca-certificates \
       poppler-utils

WORKDIR /app

COPY --from=builder /out/go-llm-proxy /usr/local/bin/go-llm-proxy

RUN mkdir -p /config /data && chown app:app /config /data

USER app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/go-llm-proxy"]
CMD ["-config", "/config/config.yaml", "-usage-db", "/data/usage.db"]
