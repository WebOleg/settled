# Slither 0.11.6 with forge from the pinned Foundry image, for the static analysis of
# the contracts (make check-contracts). Built for the machine's own platform: every
# Python package has a wheel for linux/amd64 and linux/arm64, pinned by version and
# sha256, and each base image is pinned by digest.
FROM ghcr.io/foundry-rs/foundry:v1.10.1@sha256:477ed6a2574fcec250633915433201994a92dc7476697cda02adf684d9b42092 AS foundry

FROM python:3.12-slim@sha256:191083cdf6c7bda8c76158983163ead5d5cf7b77cf3033e805bb0f5296f51b4b
COPY --from=foundry /usr/local/bin/forge /usr/local/bin/forge
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --no-deps --only-binary=:all: --require-hashes -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt
ENTRYPOINT ["slither"]
