# Multi-stage: compile veilfs from source so the demo is self-contained.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/veilfs .

FROM debian:bookworm-slim
# bash is part of `essential` packages so it's already present on
# debian:bookworm-slim, but list it explicitly to keep the intent
# visible and survive future base-image changes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        bash \
        fuse3 \
        ca-certificates \
        less \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/veilfs /usr/local/bin/veilfs
COPY examples/docker-demo/veil-entrypoint.sh /usr/local/bin/veil-entrypoint
RUN chmod +x /usr/local/bin/veil-entrypoint

ENV VEILFS_SOURCE=/work-source
ENV VEILFS_TARGET=/work
WORKDIR /work

ENTRYPOINT ["/usr/local/bin/veil-entrypoint"]
CMD ["bash"]
