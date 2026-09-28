# The panel is a Vue SPA that go:embed pulls into the binary, so the bundle has
# to exist before `go build`. Built here instead of committed to the repo.
FROM node:22-alpine AS web

WORKDIR /web

COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build

WORKDIR /src

# The module cache is warmed before the sources land, so an edited file does not
# re-download the SQLite driver.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /web/dist ./web/dist

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/qgroup-bot ./cmd/qgroup-bot

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -H -u 10001 app

COPY --from=build /out/qgroup-bot /usr/local/bin/qgroup-bot

USER app
EXPOSE 8092
ENTRYPOINT ["/usr/local/bin/qgroup-bot"]
