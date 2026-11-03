#!/bin/bash
# Ratio alerts against a REAL HyperDX 2.35.0 (ClickStack 3.2.0, as the CMP ships it), on a
# disposable kind cluster. alert-provider runs from its source tree (go run), its bootstrap
# provisions HyperDX's admin and keys exactly as the installer's Job does.
#
# Data: OTLP DELTA sums of traces.span.metrics.calls, posted through the real ClickHouse exporter,
# every 20s: 97 rows' worth with krateo.sli.error="false" and 3 with "true", so any window's error
# share is exactly 3/100. Checks:
#   - HyperDX's own ratio (charts/series, seriesReturnType ratio) over a fixed window equals K/N
#     counted straight from ClickHouse for that window, and equals 0.03;
#   - threshold 0.01 fires (ALERT), 0.05 does not (OK), with sum and with increase;
#   - an idle series (no rows at all) does not fire on 0/0;
#   - the tiles alert-provider wrote carry two selects and asRatio.
# Test values only; nothing is printed that authenticates.
#
#   hack/hyperdx-proof/proof.sh          (KEEP=1 keeps the kind cluster; WORK=dir keeps the run's files)
set -uo pipefail
P=$(cd "$(dirname "$0")" && pwd)
AP=$(cd "$P/../.." && pwd)
W=${WORK:-$(mktemp -d)}; mkdir -p $W; echo "work dir: $W"
KC=$W/kubeconfig; NS=hdx-proof; FAILS=0; N=0
k() { kubectl --kubeconfig $KC "$@"; }
pass() { N=$((N+1)); echo "PASS  $*"; }; fail() { N=$((N+1)); FAILS=$((FAILS+1)); echo "FAIL  $*"; }
ts() { date -u +%H:%M:%SZ; }
PIDS=(); cleanup() { for p in "${PIDS[@]:-}"; do kill $p 2>/dev/null; done; }; trap cleanup EXIT

echo "== cluster ($(ts))"
if kind get clusters | grep -qx hdx-proof; then kind export kubeconfig --name hdx-proof --kubeconfig $KC >/dev/null 2>&1
else kind create cluster --name hdx-proof --image kindest/node:v1.36.4 --kubeconfig $KC --wait 300s >/dev/null 2>&1 || { echo "kind create failed"; exit 2; }; fi
[ "$(k config current-context)" = kind-hdx-proof ] || { echo "wrong context"; exit 2; }

echo "== data stack ($(ts))"
k apply -f $P/stack.yaml >/dev/null
k -n $NS rollout status deploy/clickhouse deploy/mongo deploy/otel-collector --timeout 900s >/dev/null || { echo "stack not ready"; exit 2; }

echo "== HyperDX 2.35.0 ($(ts))"
# ClickStack 3.2.0 is the chart krateo-observability wraps; pinned by digest.
CS_SHA=f92d4a22c950e672981c6caa8d106e63a71fc88441cdb054a7cfef13fa17be95
curl -sfL -o $W/clickstack-3.2.0.tgz https://github.com/ClickHouse/ClickStack-helm-charts/releases/download/clickstack-3.2.0/clickstack-3.2.0.tgz
[ "$(shasum -a 256 $W/clickstack-3.2.0.tgz | cut -d' ' -f1)" = "$CS_SHA" ] || { echo "clickstack chart digest mismatch"; exit 2; }
rm -rf $W/charts && mkdir -p $W/charts && tar xzf $W/clickstack-3.2.0.tgz -C $W/charts
helm --kubeconfig $KC upgrade --install hdx $W/charts/clickstack -n $NS -f $P/clickstack-values.yaml --wait --timeout 20m >/dev/null \
  || { echo "HyperDX not ready"; k -n $NS get pods; exit 2; }
img=$(k -n $NS get deploy hdx-clickstack-app -o jsonpath='{.spec.template.spec.containers[0].image}')
[ "$img" = "docker.hyperdx.io/hyperdx/hyperdx:2.35.0" ] && pass "HyperDX image is $img (the CMP's)" || fail "HyperDX image $img"

echo "== CRDs ($(ts))"
rm -rf $W/.crds && cp -R $AP/helm/alert-provider-crds $W/.crds
perl -pi -e 's/^(version|appVersion): .*/$1: 0.0.0-proof/' $W/.crds/Chart.yaml   # CHART_VERSION is stamped at release
helm template c $W/.crds | k apply --server-side -f - >/dev/null
HELM_CACHE_HOME=$W/.helm/c HELM_CONFIG_HOME=$W/.helm/f HELM_DATA_HOME=$W/.helm/d \
  helm template x oci://ghcr.io/krateo-platformops/charts/incident-provider-crds --version 0.6.0 2>/dev/null \
  | python3 -I $P/crds-only.py \
  | k apply --server-side -f - >/dev/null
k wait --for condition=Established crd --all --timeout 120s >/dev/null && k get crd incidents.observability.krateo.io alerts.observability.krateo.io metrics.observability.krateo.io >/dev/null 2>&1 \
  && pass "Alert, Metric and Incident CRDs established" || fail "CRDs"

# CEL rules of the new field, at the real apiserver.
bad=$(cat <<'EOF'
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: cel-ratio-without-metric, namespace: hdx-proof}
spec: {where: "1=1", interval: 5m, threshold: 1, thresholdType: above, ratio: {labels: {a: b}}}
EOF
)
cel() { local out; for i in $(seq 1 20); do out=$(echo "$1" | k apply --dry-run=server -f - 2>&1); case "$out" in *"$2"*) return 0;; esac; sleep 3; done; return 1; }
cel "$bad" 'requires metricRef' && pass "CRD refuses ratio without metricRef" || fail "CEL: ratio without metricRef admitted"
bad2=$(cat <<'EOF'
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: cel-ratio-p95, namespace: hdx-proof}
spec: {metricRef: {name: span-calls}, aggregation: p95, interval: 5m, threshold: 1, thresholdType: above, ratio: {labels: {a: b}}}
EOF
)
cel "$bad2" 'sum or increase' && pass "CRD refuses a ratio of p95" || fail "CEL: ratio of p95 admitted"
bad3=$(printf '%s\n' 'apiVersion: observability.krateo.io/v1alpha1' 'kind: Alert' 'metadata: {name: cel-between-no-max, namespace: hdx-proof}' \
  'spec: {where: "1=1", interval: 5m, threshold: 1, thresholdType: between}')
cel "$bad3" 'thresholdMax is the upper bound' && pass "CRD refuses between without thresholdMax" || fail "CEL: between without thresholdMax admitted"
bad4=$(printf '%s\n' 'apiVersion: observability.krateo.io/v1alpha1' 'kind: Alert' 'metadata: {name: cel-max-below-min, namespace: hdx-proof}' \
  'spec: {where: "1=1", interval: 5m, threshold: 5, thresholdMax: 1, thresholdType: between}')
cel "$bad4" 'at least threshold' && pass "CRD refuses thresholdMax below threshold" || fail "CEL: thresholdMax < threshold admitted"

echo "== port-forwards ($(ts))"
k -n $NS port-forward deploy/hdx-clickstack-app 13000:3000 18000:8000 >/dev/null 2>&1 & PIDS+=($!)
k -n $NS port-forward svc/otel-collector 14318:4318 >/dev/null 2>&1 & PIDS+=($!)
k -n $NS port-forward svc/clickhouse 18123:8123 >/dev/null 2>&1 & PIDS+=($!)
for i in $(seq 1 60); do curl -s -o /dev/null http://127.0.0.1:18000/health && curl -s -o /dev/null http://127.0.0.1:18123/ping && break; sleep 2; done

echo "== feed: 97 ok + 3 failing per post, DELTA, every 20s ($(ts))"
feed() {
  while true; do
    now=$(python3 -I -c 'import time; print(time.time_ns())'); start=$((now - 20000000000))
    python3 -I - "$start" "$now" <<'PY' | curl -s -o /dev/null -X POST -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:14318/v1/metrics
import json, sys
start, now = sys.argv[1:3]
def a(k, v): return {"key": k, "value": {"stringValue": v}}
def dp(err, n): return {"attributes": [a("service.name", "snowplow"), a("span.name", "GET /call"), a("status.code", "STATUS_CODE_UNSET"),
                                       a("http.route", "/call"), a("krateo.sli.error", err)],
                        "startTimeUnixNano": start, "timeUnixNano": now, "asInt": str(n)}
print(json.dumps({"resourceMetrics": [{"resource": {"attributes": [a("service.name", "snowplow")]},
  "scopeMetrics": [{"scope": {"name": "spanmetricsconnector"}, "metrics": [{"name": "traces.span.metrics.calls", "unit": "",
    "sum": {"aggregationTemporality": 1, "isMonotonic": True, "dataPoints": [dp("false", 97), dp("true", 3)]}}]}]}]}))
PY
    sleep 20
  done
}
feed & PIDS+=($!)
sleep 45
rows=$(curl -s "http://127.0.0.1:18123/?query=SELECT%20count()%20FROM%20otel_metrics_sum%20WHERE%20MetricName%3D'traces.span.metrics.calls'%20AND%20AggregationTemporality%3D1")
[ "${rows:-0}" -gt 0 ] && pass "the exporter wrote $rows DELTA rows (AggregationTemporality=1)" || fail "no rows written"

echo "== bootstrap HyperDX keys with alert-provider's own bootstrap ($(ts))"
k -n $NS get secret hyperdx-api-token >/dev/null 2>&1 || {
  PW=$(python3 -I -c 'import secrets; print("Kp9!" + secrets.token_urlsafe(18))')
  ( cd $AP && env KUBECONFIG=$KC HYPERDX_URL=http://127.0.0.1:13000 HYPERDX_ADMIN_EMAIL=admin@hdx-proof.test \
      HYPERDX_ADMIN_PASSWORD="$PW" NAMESPACE=$NS SECRET_NAME=hyperdx-api-token go run . bootstrap >/dev/null 2>&1 )
  unset PW; }
k -n $NS get secret hyperdx-api-token -o jsonpath='{.data}' | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin)))' | grep -q accessKey \
  && pass "bootstrap wrote hyperdx-api-token (keys only checked)" || { fail "bootstrap"; exit 1; }
key() { k -n $NS get secret hyperdx-api-token -o jsonpath='{.data.accessKey}' | base64 -d; }

echo "== alert-provider reconciler from source ($(git -C $AP rev-parse --short HEAD)) ($(ts))"
for old in $(lsof -nP -t -iTCP:18080 -sTCP:LISTEN 2>/dev/null); do kill $old; done
( cd $AP && go build -o $W/.bin/alert-provider . ) || { fail "build the test binary"; exit 1; }
env KUBECONFIG=$KC NAMESPACE=$NS HYPERDX_API_URL=http://127.0.0.1:18000 HYPERDX_ACCESS_KEY="$(key)" \
    RECONCILE_INTERVAL=15 WEBHOOK_TARGET_URL=http://krateo-alert-provider.hdx-proof.svc:8080/webhook PORT=18080 DEFAULT_ALERTS_JSON='[]' \
    $W/.bin/alert-provider > $W/alert-provider.log 2>&1 & APID=$!; PIDS+=($APID)
sleep 5; kill -0 $APID 2>/dev/null && pass "alert-provider ($(git -C $AP rev-parse --short HEAD)) is running" || { fail "alert-provider exited: $(tail -1 $W/alert-provider.log | cut -c1-200)"; exit 1; }

cat <<EOF | k apply -f - >/dev/null
apiVersion: observability.krateo.io/v1alpha1
kind: Metric
metadata: {name: span-calls, namespace: $NS}
spec: {metricName: traces.span.metrics.calls, type: sum, description: span-metrics calls (DELTA)}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-fires-sum, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 0.029, thresholdType: above,
       labels: {service.name: snowplow, krateo.sli.error: "true"}, ratio: {labels: {service.name: snowplow}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-fires-increase, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: increase, interval: 5m, threshold: 0.029, thresholdType: above,
       labels: {service.name: snowplow, krateo.sli.error: "true"}, ratio: {labels: {service.name: snowplow}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-ok, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 0.031, thresholdType: above,
       labels: {service.name: snowplow, krateo.sli.error: "true"}, ratio: {labels: {service.name: snowplow}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-idle, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 0.01, thresholdType: above,
       labels: {service.name: ghost, krateo.sli.error: "true"}, ratio: {labels: {service.name: ghost}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-between-fires, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 0.02, thresholdMax: 0.04, thresholdType: between,
       labels: {service.name: snowplow, krateo.sli.error: "true"}, ratio: {labels: {service.name: snowplow}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-between-ok, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 0.04, thresholdMax: 0.05, thresholdType: between,
       labels: {service.name: snowplow, krateo.sli.error: "true"}, ratio: {labels: {service.name: snowplow}}}
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata: {name: ratio-idle-below, namespace: $NS}
spec: {metricRef: {name: span-calls}, aggregation: sum, interval: 5m, threshold: 1, thresholdType: below,
       labels: {service.name: ghost, krateo.sli.error: "true"}, ratio: {labels: {service.name: ghost}}}
EOF
state() { k -n $NS get alerts.observability.krateo.io $1 -o jsonpath='{.status.state}' 2>/dev/null; }
phase() { k -n $NS get alerts.observability.krateo.io $1 -o jsonpath='{.status.phase}' 2>/dev/null; }

echo "== waiting for HyperDX to evaluate (up to 15 min) ($(ts))"
T0=$(date +%s); prev=""
while [ $(( $(date +%s) - T0 )) -lt 900 ]; do
  line="fires-sum=$(state ratio-fires-sum)/$(phase ratio-fires-sum) fires-increase=$(state ratio-fires-increase) ok=$(state ratio-ok) idle=$(state ratio-idle)"
  [ "$line" != "$prev" ] && { echo "+$(( $(date +%s)-T0 ))s $line"; prev="$line"; }
  [ "$(state ratio-fires-sum)" = ALERT ] && [ "$(state ratio-fires-increase)" = ALERT ] && [ "$(state ratio-between-fires)" = ALERT ] && break
  sleep 15
done
sleep 90   # at least one more evaluation of every alert after the firing ones turned
[ "$(state ratio-fires-sum)" = ALERT ] && pass "3% failing, threshold 2.9% (sum): ALERT" || fail "ratio-fires-sum: $(state ratio-fires-sum)/$(phase ratio-fires-sum)"
[ "$(state ratio-fires-increase)" = ALERT ] && pass "3% failing, threshold 2.9% (increase on DELTA): ALERT" || fail "ratio-fires-increase: $(state ratio-fires-increase)"
[ "$(state ratio-ok)" = OK ] && pass "3% failing, threshold 3.1%: OK, so the evaluated value is in [0.029, 0.031): a ratio, not the error count" || fail "ratio-ok: $(state ratio-ok)"
[ "$(state ratio-between-fires)" = ALERT ] && pass "3% failing, between [2%, 4%] (thresholdMax reaches HyperDX): ALERT" || fail "ratio-between-fires: $(state ratio-between-fires)/$(phase ratio-between-fires)"
[ "$(state ratio-between-ok)" = OK ] && pass "3% failing, between [4%, 5%]: OK" || fail "ratio-between-ok: $(state ratio-between-ok)/$(phase ratio-between-ok)"
s=$(state ratio-idle); [ "$s" = OK ] && pass "idle series, 'above 0.01': evaluated and OK (an idle window reads 0, which is not above)" || fail "ratio-idle (above): state '${s:-none}'"
[ "$(phase ratio-idle-below)" = Invalid ] && k -n $NS get alerts.observability.krateo.io ratio-idle-below -o jsonpath='{.status.error}' | grep -q 'no traffic as 0' \
  && [ -z "$(k -n $NS get alerts.observability.krateo.io ratio-idle-below -o jsonpath='{.status.hyperdxAlertId}')" ] \
  && pass "idle series, 'below 1': refused (phase Invalid, idle reason), nothing pushed to HyperDX" || fail "ratio-idle-below: phase '$(phase ratio-idle-below)'"

echo "== HyperDX's totals vs ClickHouse's K and N over one fixed 5m window ($(ts))"
# HyperDX 2.35.0's external charts/series API ignores seriesReturnType (it returns both series), so
# the ratio itself is proven by the alerts above; here, that the two totals HyperDX divides are
# exactly ClickHouse's K and N for the same window (DELTA rows summed as-is).
END=$(( ($(date +%s) / 300) * 300 )); START=$((END - 300))
kn=$(curl -s "http://127.0.0.1:18123/" --data-binary "SELECT sumIf(Value, Attributes['krateo.sli.error']='true'), sum(Value) FROM otel_metrics_sum WHERE MetricName='traces.span.metrics.calls' AND Attributes['service.name']='snowplow' AND TimeUnix >= toDateTime64($START,9) AND TimeUnix < toDateTime64($END,9) FORMAT TSV")
K=$(echo "$kn" | cut -f1); NN=$(echo "$kn" | cut -f2)
src=$(printf 'header = "Authorization: Bearer %s"\n' "$(key)" | curl -s -K - http://127.0.0.1:18000/api/v2/sources | python3 -I -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print([s["id"] for s in d if s.get("kind")=="metric"][0])')
body=$(python3 -I $P/series-body.py "$src" "$START" "$END")
got=$(printf 'header = "Authorization: Bearer %s"\nheader = "Content-Type: application/json"\n' "$(key)" \
  | curl -s -K - -X POST --data-binary "$body" http://127.0.0.1:18000/api/v2/charts/series \
  | python3 -I -c 'import json,sys; d=json.load(sys.stdin)["data"]; b=[r for r in d if r.get("ts_bucket")==int(sys.argv[1])*1000]; print(b[0].get("series_0.data"), b[0].get("series_1.data")) if b else print("no-bucket")' "$START")
echo "window [$START,$END): ClickHouse K=$K N=$NN; HyperDX K,N=$got"
[ "$got" = "$K $NN" ] && python3 -I -c "import sys; K,N=float(sys.argv[1]),float(sys.argv[2]); sys.exit(0 if N>0 and abs(K/N-0.03)<1e-12 else 1)" "$K" "$NN" \
  && pass "HyperDX's totals are exactly ClickHouse's K=$K, N=$NN (K/N = 0.03)" || fail "HyperDX totals '$got' vs ClickHouse '$K $NN'"

echo "== the tiles alert-provider wrote ($(ts))"
did=$(k -n $NS get alerts.observability.krateo.io ratio-fires-sum -o jsonpath='{.status.hyperdxDashboardId}')
printf 'header = "Authorization: Bearer %s"\n' "$(key)" | curl -s -K - http://127.0.0.1:18000/api/v2/dashboards/$did \
  | python3 -I -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); d=d if isinstance(d,dict) else {}; c=(d.get("tiles") or [{}])[0].get("config",{}); sys.exit(0 if c.get("asRatio") is True and len(c.get("select",[]))==2 else 1)' \
  && pass "the tile carries two selects with asRatio: true" || fail "tile shape"
grep -c -i 'error' $W/alert-provider.log | xargs -I{} echo "alert-provider log: {} error line(s) (see alert-provider.log)"

[ "${KEEP:-0}" = 1 ] || kind delete cluster --name hdx-proof >/dev/null 2>&1
echo; echo "$((N-FAILS)) of $N checks passed"; [ $FAILS = 0 ]
