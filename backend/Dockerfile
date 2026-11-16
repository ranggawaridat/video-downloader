# syntax=docker/dockerfile:1
# Build binary Go, lalu runtime Alpine berisi yt-dlp + ffmpeg.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/worker ./cmd/worker

# CATATAN: sejak 2026-11-16 memakai channel nightly — TikTok/YouTube sering
# rusak di rilis stable. Pemblokiran IP datacenter TIDAK bisa diperbaiki
# dengan versi saja: perlu cookies (--cookies) atau proxy bersih.
FROM alpine:3.20
# yt-dlp di-pin supaya build reproducible; update lewat perubahan versi ini (CI mingguan disarankan).
RUN apk add --no-cache ca-certificates ffmpeg python3 py3-pip \
 && pip3 install --no-cache-dir --break-system-packages --pre "yt-dlp==2026.11.15.232800.dev0"
COPY --from=build /out/api /app/api
COPY --from=build /out/worker /app/worker
WORKDIR /app
EXPOSE 8080
CMD ["/app/api"]
