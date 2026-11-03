# syntax=docker/dockerfile:1

FROM golang:1.27-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/pbs-volume-backup ./cmd/pbs-volume-backup

FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS client
ARG PBS_CLIENT_VERSION=4.2.8-1
ARG PBS_CLIENT_SHA256=f20e5dd1565362f2c9bbfa892e4bda04e5e7a8fd9a14ef7c681ca716ac7c4001
ADD --checksum=sha256:${PBS_CLIENT_SHA256} \
    http://download.proxmox.com/debian/pbs-client/dists/trixie/main/binary-amd64/proxmox-backup-client-static_${PBS_CLIENT_VERSION}_amd64.deb \
    /client.deb
RUN dpkg-deb -x /client.deb /client

FROM gcr.io/distroless/static-debian13@sha256:448810357cc8e67b7208988abdba0f5ec3d3aebee0a5b13eb624979792cffc0f
ARG PBS_CLIENT_VERSION=4.2.8-1
LABEL org.opencontainers.image.source="https://github.com/prioe/pbs-volume-backup" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      one.prio.pbs-client.version="${PBS_CLIENT_VERSION}" \
      one.prio.pbs-client.source="https://git.proxmox.com/?p=proxmox-backup.git;a=commit;h=refs/tags/v${PBS_CLIENT_VERSION%-*}"
COPY --from=client /client/usr/bin/proxmox-backup-client /usr/bin/proxmox-backup-client
COPY --from=client /client/usr/share/doc/proxmox-backup-client-static/copyright /usr/share/doc/proxmox-backup-client-static/copyright
COPY --from=build /out/pbs-volume-backup /usr/bin/pbs-volume-backup
COPY LICENSE /usr/share/doc/pbs-volume-backup/LICENSE
ENTRYPOINT ["/usr/bin/pbs-volume-backup"]
