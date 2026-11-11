---
type: Configuration
title: Configuration
description: Environment and values that tune the controller.
tags: [observability, alerts]
timestamp: 2026-08-20T00:00:00Z
---

# Configuration

| key | default | meaning |
|---|---|---|
| `image.repository` | `ghcr.io/krateo-platformops/alert-provider` | controller image |
| `image.tag` | chart appVersion | image tag; pin to override |
| `config.autopilotA2aUrl` | `http://incident-agent.krateo-system.svc:8080/` | the RCA agent's A2A endpoint |
| `config.compareModelConfig` | `gemini-flash` | the kagent ModelConfig whose model names the open incident that covers a firing; provider OpenAI or Gemini |
| `config.maxCompareCandidates` | `50` | how many of an alert's newest open, analyzed incidents one comparison weighs |
| `config.incidentCoverWindow` | `24h` | Go duration an open, analyzed incident stays a comparison candidate after it opened or last covered a firing; past it, a firing it would have covered opens a new incident with a fresh RCA. `"0"` disables it |
| `config.failedAnalysisHold` | `1800` | seconds an incident whose RCA failed keeps taking its alert's firings, though it is closed; past it, a new incident gets a fresh RCA and supersedes it |
| `config.analysisRetries` | `2` | how many more times an RCA that failed transiently runs |
| `config.analysisRetryDelay` | `120` | seconds before the first RCA retry; each next one waits twice as long |
| `config.authnUrl` | `""` | authn, for the service JWT on both A2A calls; empty = unauthenticated |
| `config.reconcileInterval` | `60` | seconds between reconciler passes; each pass that finds an alert ALERT is one firing |
| `otel.enabled` | `false` | export OpenTelemetry traces (see Tracing) |
| `otel.tracesSampler` | `parentbased_always_on` | `OTEL_TRACES_SAMPLER` |
| `otel.serviceName` | `krateo-alert-provider` | `OTEL_SERVICE_NAME`, the spans' `service.name` |

The release namespace is the home namespace (`NAMESPACE`): its Alerts keep their bare names in
HyperDX and alone open Incidents, which it holds with the `hyperdx-api-token` Secret and the
ModelConfig. The provider reconciles the Alerts, and reads the Metrics, of every namespace: the
Role `krateo-alert-provider` covers the release namespace, the ClusterRole
`krateo-alert-provider-alerts` (bound to the `krateo-alert-provider` ServiceAccount) the Alerts,
their status, Metrics and Events everywhere.

Runtime env (set on the Deployment) carries these, plus `A2A_TIMEOUT` (900 s, the longest silence on the RCA stream) and
`COMPARE_TIMEOUT` (60 s, one comparison). See `helm/alert-provider/templates/deployment.yaml`.

## Tracing

Off by default. With `otel.enabled: false` the chart renders no OTel variable and the binary
registers no tracer: the pod is what it was before tracing. With `otel.enabled: true` the
Deployment gets `OTEL_ENABLED`, `OTEL_TRACING_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT=http://$(HOST_IP):4318`
(the node-local collector agent; `HOST_IP` is the `status.hostIP` fieldRef, declared first so it
expands), `OTEL_TRACES_SAMPLER`, `OTEL_SERVICE_NAME`, `SERVICE_VERSION` (the image tag, recorded
as `service.version`) and `DEPLOYMENT_NAME` (`krateo-alert-provider`, the Deployment's own name,
recorded as `k8s.deployment.name` and `service.instance.id`, as provider-runtime's design-tracing §5
has every provider record them; `OTEL_RESOURCE_ATTRIBUTES` wins over both). An `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set on the pod wins over the base
URL and is used as-is; the base URL gets `/v1/traces`. The `hyperdx-key-bootstrap` Job gets none of them: its pod template is
immutable within a release, so the switch must not change it.

One pipeline: the provider hands its TracerProvider (the redacting exporter, its resource) to
provider-runtime, whose reconciler spans every Alert pass (provider-runtime v1.15.0,
design-tracing §4, §11.3 and D8). The Alert controller starts no span of its own; it sets its
attributes on provider-runtime's.

The spans, by trace:

| span | kind | parent | attributes |
|---|---|---|---|
| `reconcile` | server | new root, one per Alert pass (provider-runtime), never a child of the Alert's `krateo.io/traceparent`: **linked** to it when the Alert has one, and to the `update` or `delete` that joined it | `k8s.object.kind` (`Alert`), `k8s.object.name`, `k8s.object.namespace`; `provider_runtime.acted` (bool `true`) on a pass that starts an `update` or `delete`, absent on an observe-only pass |
| `connect` | internal | `reconcile` | none; the webhook and source calls are under it |
| `observe` | internal | `reconcile` | `alert.namespace`, `alert.name`, `alert.hyperdx_id`, `alert.state`, `hyperdx.alerts_list.cached`, `hyperdx.throttled` |
| `update` | internal | `reconcile`, on a pass that creates the HyperDX dashboard and alert or pushes the spec; on the first such pass after a new `krateo.io/traceparent`, that value instead (below) | as `observe`, without the HyperDX flags, plus `alert.action` (`create` or `update`) |
| `delete` | internal | `reconcile`, on the finalizing pass; a new `krateo.io/traceparent`'s value instead, as `update` | `alert.namespace`, `alert.name`, `alert.hyperdx_id`, `alert.state` |
| `incident.fire` | internal | new root, **linked** to the `observe` (or creating `update`) that fired it | `alert.namespace`, `alert.name`, `incident.opened`, `incident.name`, `incident.stale_skipped` |
| `incident.compare` | internal | `incident.fire` | `incident.compare.candidates`, `incident.compare.cached`, `incident.compare.match` |
| `incident.analyze` | internal | `incident.fire`, or a new root linked to `incident.reanalyze` | `incident.namespace`, `incident.name`, `incident.analysis.attempts` |
| `incident.reanalyze` | internal | root, one per reanalysis request | `incident.namespace`, `incident.name`, `incident.reanalysis.count` |
| `<METHOD> /webhook` | server | root, or the caller's `traceparent` | otelhttp's; no span for `GET /healthz` |
| `HTTP GET`, `HTTP POST`, … | client | the span that made the call | otelhttp's; HyperDX, A2A, authn, the comparison model, the bootstrap session |

provider-runtime never calls the Alert's `Create`: Observe never reports the HyperDX alert missing,
so the creating pass is an `update` span with `alert.action=create`. provider-runtime marks
`observe`, `update` and `delete` Error when the call fails, and `reconcile` when the pass does.

An Alert stamped with `krateo.io/traceparent` (the composition-dynamic-controller stamps what it
applies) keeps its passes in traces of their own: each `reconcile` links to the stamp and none is
its child, so an observe-only pass never enters the stamper's trace. Only the first pass that acts
on a new value joins it, and only its act: that `update` (or `delete`) is the stamp's child, with
the HyperDX calls under it, carries the `k8s.object.*` attributes as well as the Alert's, and links
back to its `reconcile`, which links to it. provider-runtime records the value in
`krateo.io/traceparent-continued` just before that act, or when its 10-minute window closes with
none, and no later act joins it.

The `incident.` span names are a contract: observability's tail sampler keeps whole every trace
holding a span named `incident.*` and samples the routine Alert loop at about 5%, so a rename must
be coordinated with observability (pinned by `TestTheIncidentSpanNamesAreTheSamplerContract`).
From otel-collector-tailsampling 0.1.11 it also keeps every trace whose `reconcile` carries
`provider_runtime.acted=true`, so every pass that changes HyperDX keeps its reconcile
(provider-runtime#74; pinned by `wantActed` in the Alert controller's tracing tests).

The Incidents carry the trace on to incident-provider, which reconciles them with provider-runtime
(design-tracing §6.2, §6.5, §11.3 step 5, D8). From provider-runtime v1.14.1 its reconciles link to
the Incident's `krateo.io/traceparent`, and only the act of its first pass that acts on a new value
(the `update` that persists the status or starts a check Pod) is that value's child:

- the create, in `Analyzing`, carries `krateo.io/traceparent` (and `krateo.io/tracestate` when there
  is one) from `incident.fire`;
- before the status write that sets `Open` with the `howToFix` scripts, a merge patch of the
  annotations alone writes a fresh value from the current `incident.analyze` (a missing tracestate
  is written as null, so a stale one never stays). The status subresource drops metadata, so it
  cannot carry it, and the patch goes first: incident-provider's pass that sees the scripts sees
  the new value, and its `update`, which starts the check Pod, is a child of the analysis, in the
  trace the tail sampler keeps whole. A reanalysis does the same from its own `incident.analyze`. A failed patch
  is logged and the analysis is still written.

With tracing off neither is written.

A firing and a reanalysis outlive the reconcile that starts them (an RCA takes minutes), so they
run under the process's context, not the reconcile's: the reconcile timeout never cuts them short
and a shutdown still stops them. Their span is therefore a new trace linked to the reconcile's
span, not a child of a span that ended minutes earlier, and each reconcile trace stays as long as
the reconcile.

No span records a header, a body, a prompt, a completion or a token. Every URL attribute is
exported without its query string, fragment or userinfo, and a Server span without its raw path.
`traceparent` and `baggage` go only to in-cluster hosts (a bare Service name, `*.svc`,
`*.cluster.local`, a loopback or private IP): a comparison sent straight to Gemini or OpenAI still
gets its client span, with no trace header. The Kubernetes client is not instrumented, so watches
and cache reads make no spans.

A log record written inside traced work carries `trace_id` and `span_id`, the span it ran in
(provider-runtime v1.13.0's `logging.WithContext`). It names the innermost span open when it was
written: a line logged during `observe` names `observe`, not `reconcile`, a recreated webhook's
line names `connect`, and a firing's or an RCA's lines name their own `incident.fire` or
`incident.analyze`, in their own trace, not the reconcile that started them. provider-runtime's own
per-reconcile lines name `reconcile`. Lines written outside any span carry neither: startup, the default-Alert
seeding, the sweep of incidents a restart left `Analyzing`, controller-runtime's own lines (logr
carries no context), and the `hyperdx-key-bootstrap` Job, which prints plain text with tracing
off. With tracing off no record carries either.

