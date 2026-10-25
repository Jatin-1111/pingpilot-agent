# Ping Pilott agent, for hosts that run containers.
#
# The agent reports on the machine, not on the container, so it has to be given the machine:
#
#   docker run -d --name pingpilot-agent --restart unless-stopped \
#     --pid=host --net=host --uts=host \
#     -v /:/host:ro \
#     -e PINGPILOT_HOST_ROOT=/host \
#     -e PINGPILOT_TOKEN=ppa_... \
#     ghcr.io/jatin-1111/pingpilot-agent
#
#   --pid=host   sees the host's processes (names only are read) and its mount table
#   --net=host   counts the host's network interfaces, not the container's one
#   --uts=host   reports the host's name
#   -v /:/host   lets it measure the host's disks (read only); PINGPILOT_HOST_ROOT tells it where they are
#
# It still opens no port, runs as an unprivileged user, and holds nothing but the one binary.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pingpilot-agent ./cmd/pingpilot-agent

# static: no shell, no package manager, only CA certificates (for https)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pingpilot-agent /pingpilot-agent
ENV PINGPILOT_HOST_ROOT=/host
USER nonroot:nonroot
ENTRYPOINT ["/pingpilot-agent"]
CMD ["run"]
