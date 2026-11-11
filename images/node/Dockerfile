# cloudex-node: the agent that runs on a node (v0.2.2), linux/amd64. Built from
# the repository's root:  docker build -f images/node/Dockerfile .
# A static Go binary on distroless, as the nonroot user. The installer
# (scripts/node-install.sh) runs it with the Docker socket and --group-add of
# the socket's group, --sysctl net.ipv4.ip_forward=0 (no machine routes
# through it), a read-only root, and its state in the cloudex-node volume.
# Bases pinned by digest (review H5); update them together with the tags.
FROM golang:1.27@sha256:01aa404fa56b604b6f48e90c1d6e2c79fe5cd78f9592f78d80303fbe171cdcc0 AS build
ARG VERSION=dev
WORKDIR /src
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cloudex-node ./cmd/cloudex-node \
  && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/cloudex-node /usr/local/bin/cloudex-node
# A new volume mounted here takes this directory's owner, so nonroot can write it.
COPY --from=build --chown=65532:65532 /out/state /var/lib/cloudex-node
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cloudex-node"]
CMD ["run"]
