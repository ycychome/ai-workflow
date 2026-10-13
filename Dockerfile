# Trusted control service. The Docker CLI is present here because only this
# process receives the Docker socket; worker containers use the separate image.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control ./cmd/control

FROM alpine:3.22
RUN apk add --no-cache docker-cli ca-certificates tzdata \
    && addgroup -S control \
    && adduser -S -D -H -G control control \
    && mkdir -p /data /secrets \
    && chown -R control:control /data
COPY --from=build /out/control /usr/local/bin/control
ENV DATA_DIR=/data CONFIG_FILE=/secrets/config.yaml
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/control"]
