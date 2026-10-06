# The image `make demo-docker` records docs/demo.gif in: vhs with its ttyd, ffmpeg, Chromium and
# fonts, plus what demo.tape requires and that image lacks (git, python3, go.mod's Go).
# Built with no context: docker build --build-arg GO_VERSION=<go.mod's> - <scripts/demo.Dockerfile
ARG GO_VERSION
FROM golang:${GO_VERSION}-trixie AS go

FROM ghcr.io/charmbracelet/vhs:v0.12.1
RUN apt-get update && apt-get install -y --no-install-recommends git python3 && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
# No VCS stamping: the mounted repository belongs to another user, and a worktree's .git points
# at a path outside the container.
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-buildvcs=false
