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
expands), `OTEL_TRACES_SAMPLER`, `OTEL_SERVICE_NAME` and `SERVICE_VERSION` (the image tag, recorded
as `service.version`). An `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set on the pod wins over the base
URL and is used as-is; the base URL gets `/v1/traces`. The `hyperdx-key-bootstrap` Job gets none of them: its pod template is
immutable within a release, so the switch must not change it.

The spans, by trace:

| span | kind | parent | attributes |
|---|---|---|---|
| `alert.reconcile` | internal | root, one per Alert pass | `alert.namespace`, `alert.name` |
| `alert.observe` | internal | `alert.reconcile` | `alert.namespace`, `alert.name`, `alert.hyperdx_id`, `alert.state` |
| `alert.create` | internal | `alert.reconcile`, on the pass that creates the HyperDX dashboard and alert | as above |
| `alert.update` | internal | `alert.reconcile`, on a pass that pushes the spec | as above |
| `alert.delete` | internal | `alert.reconcile`, on the finalizing pass | as above |
| `incident.fire` | internal | new root, **linked** to the `alert.observe` that fired it | `alert.namespace`, `alert.name`, `incident.opened`, `incident.name`, `incident.stale_skipped` |
| `incident.compare` | internal | `incident.fire` | `incident.compare.candidates`, `incident.compare.cached`, `incident.compare.match` |
| `incident.analyze` | internal | `incident.fire`, or a new root linked to `incident.reanalyze` | `incident.namespace`, `incident.name`, `incident.analysis.attempts` |
| `incident.reanalyze` | internal | root, one per reanalysis request | `incident.namespace`, `incident.name`, `incident.reanalysis.count` |
| `<METHOD> /webhook` | server | root, or the caller's `traceparent` | otelhttp's; no span for `GET /healthz` |
| `HTTP GET`, `HTTP POST`, … | client | the action that made the call | otelhttp's; HyperDX, A2A, authn, the comparison model, the bootstrap session |

The `incident.` span names are a contract: observability's tail sampler keeps whole every trace
holding a span named `incident.*` and samples the routine Alert loop at about 5%, so a rename must
be coordinated with observability (pinned by `TestTheIncidentSpanNamesAreTheSamplerContract`).

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
written: a line logged during `alert.observe` names `alert.observe`, not `alert.reconcile`, and a
firing's or an RCA's lines name their own `incident.fire` or `incident.analyze`, in their own
trace, not the reconcile that started them. provider-runtime's own per-reconcile lines name
`alert.reconcile`. Lines written outside any span carry neither: startup, the default-Alert
seeding, the sweep of incidents a restart left `Analyzing`, controller-runtime's own lines (logr
carries no context), and the `hyperdx-key-bootstrap` Job, which prints plain text with tracing
off. With tracing off no record carries either.

