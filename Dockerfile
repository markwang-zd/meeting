FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY server.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/meeting-room-server ./server.go

FROM alpine:3.21

RUN apk add --no-cache tzdata \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/meeting-room-server ./meeting-room-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh

USER app
EXPOSE 8080

ENV LISTEN_ADDR=:8080 \
    DASHBOARD_TIME_ZONE=Asia/Shanghai \
    DASHBOARD_REFRESH_INTERVAL=5m

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
