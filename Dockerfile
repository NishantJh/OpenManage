FROM node:22-alpine AS frontend
WORKDIR /src/web
COPY web/package.json ./
RUN npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN mkdir -p cmd/openmanage/web/dist
COPY --from=frontend /src/web/dist/ ./cmd/openmanage/web/dist/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openmanage ./cmd/openmanage

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && addgroup -S openmanage && adduser -S -G openmanage openmanage
WORKDIR /app
COPY --from=backend /out/openmanage /app/openmanage
USER openmanage
EXPOSE 8080
ENV LISTEN_ADDR=:8080
ENTRYPOINT ["/app/openmanage"]
