FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /guardian-sentinel ./cmd/guardian-sentinel

FROM alpine:3.20 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /guardian-sentinel /guardian-sentinel
ENTRYPOINT ["/guardian-sentinel"]
