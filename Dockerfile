FROM golang:1.27-alpine AS build

WORKDIR /src

# No third-party dependencies, so this layer only needs go.mod to warm the cache.
COPY go.mod ./
RUN go mod download

COPY . .

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
