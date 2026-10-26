#!/usr/bin/env bash
# Smoke test for the o11y lab.
#   1. every stack component is healthy
#   2. inventory-api CRUD + order-api order flow behave correctly
#   3. telemetry from that traffic lands in VictoriaMetrics / Logs / Traces
#
# Usage: scripts/smoke.sh [--no-telemetry]
# Env:   INVENTORY_URL ORDER_URL VM_URL VL_URL VT_URL GRAFANA_URL ALLOY_URL VECTOR_URL
set -uo pipefail

INVENTORY_URL=${INVENTORY_URL:-http://localhost:8081}
ORDER_URL=${ORDER_URL:-http://localhost:8082}
VM_URL=${VM_URL:-http://localhost:8428}
VL_URL=${VL_URL:-http://localhost:9428}
VT_URL=${VT_URL:-http://localhost:10428}
GRAFANA_URL=${GRAFANA_URL:-http://localhost:3300}
ALLOY_URL=${ALLOY_URL:-http://localhost:12345}
VECTOR_URL=${VECTOR_URL:-http://localhost:8686}
CHECK_TELEMETRY=1
[[ "${1:-}" == "--no-telemetry" ]] && CHECK_TELEMETRY=0

PASS=0
FAIL=0
green() { printf '\033[32m%s\033[0m\n' "$*"; }
red() { printf '\033[31m%s\033[0m\n' "$*"; }
ok() { PASS=$((PASS + 1)); green "  ✔ $*"; }
ko() { FAIL=$((FAIL + 1)); red "  ✘ $*"; }

# req METHOD URL [BODY] -> sets STATUS and BODY. Retries 503s (chaos injection).
req() {
  local method=$1 url=$2 data=${3:-} out attempt
  for attempt in 1 2 3 4 5; do
    if [[ -n "$data" ]]; then
      out=$(curl -s -m 10 -w $'\n%{http_code}' -X "$method" -H 'Content-Type: application/json' \
        ${TRACEPARENT:+-H "traceparent: $TRACEPARENT"} -d "$data" "$url")
    else
      out=$(curl -s -m 10 -w $'\n%{http_code}' -X "$method" \
        ${TRACEPARENT:+-H "traceparent: $TRACEPARENT"} "$url")
    fi
    STATUS=${out##*$'\n'}
    BODY=${out%$'\n'*}
    [[ "$STATUS" != "503" && "$STATUS" != "502" ]] && return 0
    sleep 0.3
  done
}

expect() { # expect DESCRIPTION WANT_STATUS
  if [[ "$STATUS" == "$2" ]]; then ok "$1 ($STATUS)"; else ko "$1: want $2, got $STATUS — $BODY"; fi
}

json_field() { # json_field NAME  (first numeric/string value of a top-level key in $BODY)
  sed -nE "s/.*\"$1\":\"?([^\",}]*)\"?.*/\1/p" <<<"$BODY" | head -n1
}

wait_for() { # wait_for NAME URL [TIMEOUT_SECONDS]
  local name=$1 url=$2 timeout=${3:-90} i
  for ((i = 0; i < timeout; i += 2)); do
    if curl -sf -m 3 -o /dev/null "$url"; then ok "$name healthy"; return 0; fi
    sleep 2
  done
  ko "$name not healthy at $url after ${timeout}s"
  return 1
}

poll() { # poll DESCRIPTION TIMEOUT CMD... (passes when CMD exits 0)
  local desc=$1 timeout=$2 i
  shift 2
  for ((i = 0; i < timeout; i += 3)); do
    if "$@" >/dev/null 2>&1; then ok "$desc"; return 0; fi
    sleep 3
  done
  ko "$desc (timed out after ${timeout}s)"
}

echo "== 1. Component health"
wait_for victoriametrics "$VM_URL/health"
wait_for victorialogs "$VL_URL/health"
wait_for victoriatraces "$VT_URL/health"
wait_for alloy "$ALLOY_URL/-/ready"
wait_for vector "$VECTOR_URL/health"
wait_for grafana "$GRAFANA_URL/api/health"
wait_for inventory-api "$INVENTORY_URL/healthz"
wait_for order-api "$ORDER_URL/healthz"

echo "== 2. inventory-api CRUD"
SKU="SMOKE-$(date +%s)-$RANDOM"
req POST "$INVENTORY_URL/items" "{\"sku\":\"$SKU\",\"name\":\"Smoke item\",\"price\":12.5,\"stock\":10}"
expect "create item" 201
ITEM_ID=$(json_field id)
req GET "$INVENTORY_URL/items/$ITEM_ID";                                           expect "get item" 200
req PUT "$INVENTORY_URL/items/$ITEM_ID" '{"name":"Smoke item v2","price":15,"stock":5}'; expect "update item" 200
req GET "$INVENTORY_URL/items";                                                    expect "list items" 200
req POST "$INVENTORY_URL/items/$ITEM_ID/reserve" '{"quantity":2}';                 expect "reserve stock" 200
req POST "$INVENTORY_URL/items/$ITEM_ID/reserve" '{"quantity":999}';               expect "reserve beyond stock rejected" 409
req POST "$INVENTORY_URL/items" '{"sku":""}';                                      expect "invalid create rejected" 400
req GET "$INVENTORY_URL/items/999999999";                                          expect "missing item" 404

echo "== 3. order-api -> inventory-api"
req GET "$ORDER_URL/catalog"; expect "catalog via inventory-api" 200
# Fixed trace id so we can look this exact request up in VictoriaTraces.
TRACE_ID=$(LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 32)
TRACEPARENT="00-${TRACE_ID}-$(LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 16)-01"
req POST "$ORDER_URL/orders" "{\"item_id\":$ITEM_ID,\"quantity\":1}"
unset TRACEPARENT
expect "create order (trace $TRACE_ID)" 201
ORDER_ID=$(json_field id)
req GET "$ORDER_URL/orders/$ORDER_ID";                                      expect "get order" 200
req GET "$ORDER_URL/orders";                                                expect "list orders" 200
req POST "$ORDER_URL/orders" "{\"item_id\":$ITEM_ID,\"quantity\":999}";     expect "order beyond stock rejected" 409
req POST "$ORDER_URL/orders" '{"item_id":999999999,"quantity":1}';          expect "order for missing item" 404
req DELETE "$INVENTORY_URL/items/$ITEM_ID";                                 expect "delete item" 204

if [[ $CHECK_TELEMETRY == 1 ]]; then
  echo "== 4. Telemetry landed (can take ~30s: batching + 10s metric export)"
  vm_has() { curl -sf --get "$VM_URL/api/v1/query" --data-urlencode "query=$1" | grep -q '"value"'; }
  vl_has() { curl -sf "$VL_URL/select/logsql/query" --data-urlencode "query=$1" --data-urlencode 'limit=1' | grep -q .; }
  vt_trace_has() { curl -sf "$VT_URL/select/jaeger/api/traces/$TRACE_ID" | grep -q "\"serviceName\":\"$1\""; }

  poll "metrics: order-api HTTP server histogram in VictoriaMetrics" 90 \
    vm_has 'http_server_request_duration_seconds_count{service_name="order-api"}'
  poll "metrics: inventory-api DB client histogram in VictoriaMetrics" 90 \
    vm_has 'db_client_operation_duration_seconds_count{service_name="inventory-api"}'
  poll "metrics: business counter orders_created_total" 90 vm_has 'orders_created_total'
  poll "metrics: stack self-monitoring (alloy/vector/vl/vt scraped)" 90 \
    vm_has 'count(up{job=~"alloy|vector|victorialogs|victoriatraces"} == 1) == 4'
  poll "logs: order-api log for trace $TRACE_ID in VictoriaLogs" 90 \
    vl_has "service:order-api AND trace_id:$TRACE_ID"
  poll "logs: infra container logs in VictoriaLogs" 90 vl_has 'service:victoriametrics'
  poll "traces: trace $TRACE_ID contains order-api spans" 90 vt_trace_has order-api
  poll "traces: trace $TRACE_ID propagated into inventory-api" 90 vt_trace_has inventory-api
fi

echo
if ((FAIL > 0)); then
  red "SMOKE FAILED: $PASS passed, $FAIL failed"
  exit 1
fi
green "SMOKE PASSED: $PASS checks"
echo "Grafana: $GRAFANA_URL  (Explore -> VictoriaTraces -> trace id $TRACE_ID)"
