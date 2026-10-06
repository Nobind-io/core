FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git build-base

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY proxy.go .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o proxy proxy.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/proxy .
EXPOSE 8080
CMD ["./proxy"]
