---
type: Example
title: Default alerts
description: The Alert CRs the chart seeds by default, and the rules for writing your own.
tags: [observability, alerts]
timestamp: 2026-08-24T00:00:00Z
---

# Default alerts

The chart seeds **no** alerts by default (`config.defaultAlerts: []`): the platform's default alert
set ships, tuned, in krateo-observability, and a second set here would duplicate it. Seeding only
creates absent Alerts and never deletes one, so an install upgraded from a version that seeded keeps
those copies until they are deleted.

[`default-alerts.yaml`](./default-alerts.yaml) keeps the three alerts the chart used to seed, as
applyable `Alert` CRs for reference and editing:

| alert | fires on | interval / threshold |
|---|---|---|
| `krateo-composition-reconcile-error` | control-plane compositions failing to reconcile | 15m / `>2` |
| `krateo-platform-crashloop` | a platform pod crash-looping | 15m / `>3` |
| `krateo-platform-image-pull-failure` | a platform pod cannot pull its image | 15m / `>2` |

Each firing of an Alert in the provider's namespace launches an incident-agent RCA, so a default
set should be **small, platform-scoped and high-threshold** — the goal is a few signals that never storm, not broad coverage.
Add your own by applying more `Alert` CRs.

## Writing an Alert

`spec.where` is **ClickHouse SQL over `otel_logs`** (the reconciler pins `whereLanguage=sql`). Three rules:

1. **SQL, not Lucene.**
2. **Filter on `Body` / `ResourceAttributes`** — this OTel pipeline leaves `SeverityText` empty, so a
   severity predicate counts zero and never fires.
3. **Scope every alert** — either to a namespace (`ResourceAttributes['k8s.namespace.name'] = '...'`) or
   with a `ServiceName NOT IN (...)` self-exclusion — so it never matches the observability stack's own
   log of the query it runs (which would make it diagnose itself).

`thresholdType: above` is strictly-greater, so `threshold: 0` fires on the first matching row; give
defaults a real threshold over a window.

## Alerting on spans

`spec.source: traces` makes `where` (and `value`) read **`otel_traces`**, one row per span, instead
of the logs. A span carries its outcome in columns, so no Body parsing is needed:

```yaml
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata:
  name: snowplow-failing-calls
  namespace: krateo-system
spec:
  source: traces
  where: "ServiceName = 'snowplow' AND StatusCode = 'Error'"
  interval: 15m
  threshold: 5
  thresholdType: above
  message: snowplow calls are failing
```

A traces Alert needs a `where`, and cannot take `metricRef`, `labels` or `ratio`. With `value` and
`aggregation` it aggregates a value read from each span instead of counting them, e.g. `value:
Duration / 1e6` with `aggregation: p95` for latency in milliseconds.

```sh
kubectl apply -f default-alerts.yaml
kubectl -n krateo-system get alerts
```

For a full fault-injection lab (13 alerts paired with a `breakers.yaml` that injects each fault), see
[`krateo-agentiko/incident-agent` → `examples/alert-lab/`](https://github.com/krateo-agentiko/incident-agent/tree/main/examples/alert-lab).

## Alerting per container

`spec.groupBy` makes a metric Alert evaluate one series per group, so one container that is not
ready fires however many are ready. A container not ready for five minutes in a row:

```yaml
apiVersion: observability.krateo.io/v1alpha1
kind: Metric
metadata:
  name: k8s-container-ready
  namespace: krateo-system
spec:
  metricName: k8s.container.ready   # k8sclusterreceiver: 1 ready, 0 not
  type: gauge
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata:
  name: sre-pod-not-ready
  namespace: krateo-system
spec:
  metricRef: {name: k8s-container-ready}
  aggregation: max
  groupBy: [k8s.namespace.name, k8s.pod.name, k8s.container.name]
  interval: 1m
  consecutiveWindows: 5
  threshold: 1
  thresholdType: below
  message: A container is not ready.
```

How HyperDX evaluates it:

- **A gauge is its last sample per interval**, per series; `aggregation` combines the series of one
  group. With the container in the keys a group is one series, so `max` and `min` agree. Group by
  pod alone and `max` hides a pod's one unready container: use `min` then.
- **`consecutiveWindows` is counted per group.** A container unready for four minutes is PENDING
  and notifies nothing; the fifth fires.
- **One OK interval resolves** the group (and notifies, if it had fired).
- **A gap restarts the count**: an interval without a sample is skipped, not counted as unready.
- The keys are read off the point or, as here, its resource; a key matches either.
- The notification names the firing series' keys and values (`Group: …`), and the provider's
  webhook log carries that line: `[webhook] '🚨 sre-pod-not-ready' ALERT group="…" eventId=…`.

HyperDX never resolves a series that stops reporting (a deleted pod), and its API shows only the
roll-up state, so the provider lists the series that are ALERT in `status.groups`:

```yaml
status:
  state: ALERT
  groups:
  - group: {k8s.namespace.name: krateo-system, k8s.pod.name: snowplow-5c4954d6fc-s5rvj, k8s.container.name: snowplow}
    value: 0
    since: "2026-11-08T15:35:00Z"
```

A series drops out of `status.groups` once it is ready again or gone. It is judged one interval
late (the newest minute may not be ingested yet), so it can trail HyperDX's own state by a minute.
