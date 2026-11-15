# syntax=docker/dockerfile:1
# Build binary Go, lalu runtime Alpine berisi yt-dlp + ffmpeg.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/worker ./cmd/worker

FROM alpine:3.20
# yt-dlp di-pin supaya build reproducible; update lewat perubahan versi ini (CI mingguan disarankan).
RUN apk add --no-cache ca-certificates ffmpeg python3 py3-pip \
 && pip3 install --no-cache-dir --break-system-packages "yt-dlp==2024.12.13"
COPY --from=build /out/api /app/api
COPY --from=build /out/worker /app/worker
WORKDIR /app
EXPOSE 8080
CMD ["/app/api"]
