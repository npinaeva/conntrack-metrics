FROM golang:1.22 AS builder
# golang envs
ARG GOARCH="amd64"
ARG GOOS=linux
ENV CGO_ENABLED=0

WORKDIR /go/src/app
COPY ./ ./
RUN CGO_ENABLED=0 go build -o /go/bin/conntrack-metrics .

FROM debian:bookworm
COPY --from=builder --chown=root:root /go/bin/conntrack-metrics /usr/local/bin/conntrack-metrics
