FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -trimpath -ldflags="-s -w" -o /out/go-llm-proxy .

FROM alpine:3.21

RUN addgroup -S app && adduser -S -G app app \
    && apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /out/go-llm-proxy /usr/local/bin/go-llm-proxy
COPY config.yaml.example /app/config.yaml.example

USER app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/go-llm-proxy"]
CMD ["-config", "/config/config.yaml"]
