---
type: Usage
title: Install and use
description: Install the chart, point HyperDX alerts at the webhook, read incidents in the portal.
tags: [observability, alerts]
timestamp: 2026-08-20T00:00:00Z
---

# Usage

```sh
helm install alert-provider \
  oci://ghcr.io/krateo-platformops/charts/alert-provider --version <tag> \
  --namespace krateo-system
```

Apply `Alert` CRs (see [`examples/alerts/`](../examples/alerts/)). The reconciler creates one
HyperDX alert per CR, named after its `metadata.name`, and the shared webhook that posts to
`http://krateo-alert-provider.krateo-system.svc:8080/webhook`, which only acknowledges. Every
pass (60 s) that finds a HyperDX alert ALERT is a firing of its CR. Each firing is recorded on an
`Incident`, so incident-controller's CRD chart must be installed first; without it a firing is
logged and lost. Comparing a firing with the alert's open incidents is one call to the model of the kagent
ModelConfig `config.compareModelConfig`.

## Alert on a metric

An Alert can aggregate a collected metric instead of counting log rows. Name the metric once as a
`Metric`, then reference it:

```yaml
apiVersion: observability.krateo.io/v1alpha1
kind: Metric
metadata:
  name: llm-cost-usd
  namespace: krateo-system
spec:
  metricName: agentgateway_gen_ai_client_cost_usd_total
  type: sum
  unit: USD
  description: Cumulative LLM spend per tenant, priced by the agentgateway catalog.
---
apiVersion: observability.krateo.io/v1alpha1
kind: Alert
metadata:
  name: llm-budget-acme
  namespace: krateo-system
spec:
  displayName: LLM spend of tenant acme
  metricRef: {name: llm-cost-usd}
  aggregation: increase        # a counter's increase over the interval
  labels: {tenant: acme}       # only this tenant's points
  interval: 1d
  threshold: 5
  thresholdType: above
  message: Tenant acme spent more than $5 of LLM in a day.
```

- `aggregation` is how the points over `interval` become the compared value: `avg`, `max`, `min`,
  `sum`, `p95`, or `increase`. `increase` is a counter's (`type: sum`) increase over the window,
  with counter resets clamped, so a restarted process neither spikes nor goes negative; it is
  refused on any other type.
- `labels` are ANDed attribute matches; keys are attribute names, values are quoted for you.
- The Metric is read from the Alert's namespace. A missing one, or an aggregation that cannot
  apply to it, sets `phase: Invalid` and says why.
- The window is `interval`, at most `1d`: HyperDX evaluates an alert over its interval, so a
  budget over a longer period is expressed per day.
- The count rules under `threshold` do not apply: a metric's value can be anything.
- A metric firing opens no Incident (incidents read log records); its `status.state` is `ALERT`
  and HyperDX notifies through the webhook.

## Pause an alert

`krateo.io/paused: "true"` on an `Alert` skips its firings: no incident opens, so no comparison
or RCA runs. The reconciler still pushes the spec to HyperDX and mirrors
its state, so a paused alert can be corrected and watched before it is resumed. Incidents already
open keep their checks, and an RCA already running finishes. Any other value, or no annotation,
resumes it on the next pass.

```sh
kubectl annotate alert <name> -n krateo-system krateo.io/paused=true --overwrite
kubectl annotate alert <name> -n krateo-system krateo.io/paused-
```

The portal's Pause and Resume buttons write the same annotation with the user's token, so they need
`patch` on `alerts.observability.krateo.io`.

## Who can see alerts

The portal reads `Alert` CRs with the signed-in user's own token. The chart ships the ClusterRole
`krateo-alert-viewer` (get/list/watch on `alerts.observability.krateo.io`) with no binding and no
aggregation labels, so no user sees alerts until an admin binds it to a group. A RoleBinding
limits it to the Alerts' namespace:

```sh
kubectl create rolebinding krateo-alert-viewer-devs -n krateo-system --clusterrole=krateo-alert-viewer --group=devs
```

A ClusterRoleBinding (`kubectl create clusterrolebinding … --clusterrole=krateo-alert-viewer`)
grants it in every namespace.
