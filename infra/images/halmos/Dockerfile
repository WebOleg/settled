# Halmos 0.3.3 with forge from the pinned Foundry image, for the symbolic tests of the
# contracts (make check-contracts). Built for linux/amd64 only: Halmos pins z3-solver
# 4.12.6.0, which has no arm64 wheel, so an arm64 machine runs it under emulation. Each
# base image is pinned by digest and each Python package by version and sha256.
FROM ghcr.io/foundry-rs/foundry:v1.10.1@sha256:477ed6a2574fcec250633915433201994a92dc7476697cda02adf684d9b42092 AS foundry

FROM python:3.12-slim@sha256:191083cdf6c7bda8c76158983163ead5d5cf7b77cf3033e805bb0f5296f51b4b
COPY --from=foundry /usr/local/bin/forge /usr/local/bin/forge
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --no-deps --only-binary=:all: --require-hashes -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt
ENTRYPOINT ["halmos"]
