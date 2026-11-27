# The withdrawal payer (cmd/withdrawal-payer), built from this repository for the
# machine's own platform, with the build context cut down to its sources by the
# .dockerignore at the repository root. Modules are downloaded at build time and checked
# against go.sum. The image it runs in has no shell and no package manager, and runs as
# an unprivileged user. Each base image is pinned by digest, the same as the deposit
# indexer's.
FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=local GOFLAGS=-mod=readonly go mod download
COPY internal/vaultwatch internal/vaultwatch
COPY internal/ledger internal/ledger
COPY internal/deposits internal/deposits
COPY internal/withdrawals internal/withdrawals
COPY cmd/withdrawal-payer cmd/withdrawal-payer
RUN CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -trimpath -ldflags=-s -o /out/withdrawal-payer ./cmd/withdrawal-payer

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/withdrawal-payer /withdrawal-payer
ENTRYPOINT ["/withdrawal-payer"]
