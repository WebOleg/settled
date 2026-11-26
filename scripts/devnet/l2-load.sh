#!/usr/bin/env bash
# Sends plain transfers to the L2 sequencer at a fixed rate and measures inclusion and
# safe latency. The work is done by cmd/l2-load; this script only hands it the devnet
# mnemonic, without printing it. Flags go through: --rate, --duration, --senders.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l2-load "$@"
