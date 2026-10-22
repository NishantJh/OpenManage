FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27.3-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN mkdir -p cmd/openmanage/web/dist
COPY --from=frontend /src/web/dist/ ./cmd/openmanage/web/dist/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openmanage ./cmd/openmanage

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && addgroup -S openmanage && adduser -S -G openmanage openmanage
WORKDIR /app
COPY --from=backend /out/openmanage /app/openmanage
USER openmanage
EXPOSE 8080
ENV LISTEN_ADDR=:8080
ENTRYPOINT ["/app/openmanage"]
