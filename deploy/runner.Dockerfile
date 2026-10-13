# Prebuilt toolchain image used by isolated attempt volumes. The toolchain and
# system tools are baked into the image so default runs need no live downloads.
FROM golang:1.24-bookworm
WORKDIR /opt/warm
COPY go.mod go.sum ./
RUN go mod download \
    && chmod -R a+rX /go/pkg/mod
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 worker \
    && useradd --uid 10001 --gid 10001 --create-home --shell /bin/sh worker \
    && mkdir -p /workspace /tmp \
    && chown -R 10001:10001 /workspace /home/worker /tmp
ENV HOME=/tmp \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_GLOBAL=/dev/null \
    GOTOOLCHAIN=local \
    GOPROXY=off \
    GOSUMDB=off
WORKDIR /workspace
USER 10001:10001
CMD ["sleep", "infinity"]
