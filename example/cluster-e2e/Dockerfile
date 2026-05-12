# Multi-stage build for m7s cluster e2e.
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN cd example/cluster && \
    CGO_ENABLED=0 go build -tags 'cluster postgres s3' -o /out/m7s ./main.go

FROM alpine:3.19
RUN apk add --no-cache ca-certificates ffmpeg curl
COPY --from=builder /out/m7s /usr/local/bin/m7s
WORKDIR /app
ENTRYPOINT ["/usr/local/bin/m7s"]
