#!/usr/bin/env bash
# Run the k6 load test inside the compose network and push k6 metrics to
# VictoriaMetrics (k6_* series) so client- and server-side views line up.
#
# Usage: scripts/stress.sh [load|stress|spike] [extra k6 args...]
#   DURATION=10m scripts/stress.sh load
set -euo pipefail
cd "$(dirname "$0")/.."

DOCKER=${DOCKER:-docker}
PROFILE=${1:-load}
shift || true

exec "$DOCKER" compose --profile loadtest run --rm \
  -e PROFILE="$PROFILE" \
  -e DURATION="${DURATION:-3m}" \
  -e K6_PROMETHEUS_RW_PUSH_INTERVAL=10s \
  k6 run \
  -o experimental-prometheus-rw \
  --tag testid="${PROFILE}-$(date +%Y%m%d-%H%M%S)" \
  "$@" /scripts/load.js
