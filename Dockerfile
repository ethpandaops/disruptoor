# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -trimpath -ldflags="-s -w" \
    -o /out/disruptoor ./cmd/disruptoor

FROM alpine:3.20
RUN apk add --no-cache \
    iptables \
    ip6tables \
    ipset \
    iproute2 \
    iproute2-tc \
    conntrack-tools \
    util-linux \
    iputils \
    ca-certificates
COPY --from=build /out/disruptoor /usr/local/bin/disruptoor
EXPOSE 7700
ENTRYPOINT ["/usr/local/bin/disruptoor"]
CMD ["--addr=:7700"]
