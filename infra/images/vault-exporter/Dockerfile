# The vault exporter (cmd/vault-exporter), built from this repository for the machine's
# own platform, with the build context cut down to its sources by the .dockerignore at the
# repository root. It needs only the standard library and internal/vaultwatch. The image
# it runs in has no shell and no package manager, and runs as an unprivileged user. Each
# base image is pinned by digest.
FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/vaultwatch internal/vaultwatch
COPY cmd/vault-exporter cmd/vault-exporter
RUN CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -trimpath -ldflags=-s -o /out/vault-exporter ./cmd/vault-exporter

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/vault-exporter /vault-exporter
ENTRYPOINT ["/vault-exporter"]
