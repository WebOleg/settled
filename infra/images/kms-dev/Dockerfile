# The devnet's KMS (cmd/kms-dev), built from this repository for the machine's own
# platform, with the build context cut down to its sources by the .dockerignore at the
# repository root. Modules are downloaded at build time and checked against go.sum. The
# image it runs in has no shell and no package manager, and runs as an unprivileged
# user, who owns the keys' folder, readable by it alone (700): a new volume takes its
# owner and mode from it. Each base image is pinned by digest, the same as the
# withdrawal signer's.
FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=local GOFLAGS=-mod=readonly go mod download
COPY internal/awscreds internal/awscreds
COPY internal/kmsdev internal/kmsdev
COPY cmd/kms-dev cmd/kms-dev
RUN mkdir -m 700 -p /out/data
RUN CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -trimpath -ldflags=-s -o /out/kms-dev ./cmd/kms-dev

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kms-dev /kms-dev
COPY --from=build --chown=65532:65532 --chmod=700 /out/data /data
ENTRYPOINT ["/kms-dev"]
