---
type: Architecture
title: alert-provider — architecture
description: How a firing alert becomes Incidents, one per problem, each with an incident-agent root-cause analysis.
tags: [observability, alerts]
timestamp: 2026-08-20T00:00:00Z
---

# alert-provider

A controller (not an agent), on Krateo's provider-runtime. The reconciler mirrors each `Alert`'s HyperDX state about every
60 s (`config.reconcileInterval`), and each pass that finds it ALERT is one firing. A firing is
recorded on an `Incident` (`observability.krateo.io/v1alpha1`, the CRD incident-controller ships):
an open incident of the same problem counts it, or a new incident opens and **incident-agent**
root-causes it over A2A. One LLM call, on the fleet's model, names the open incident it belongs
to; a firing with the same incidents and no record the last answered call did not see keeps that
call's verdict. Firings
and analyses run off the reconcile thread. HyperDX's webhook (`POST /webhook`) is acked 202 and
only logged: a HyperDX alert needs a channel, and the reconciler already fires it.

It lives in `krateo-platformops` (not `krateo-agentiko`) because it is observability
plumbing keyed on the platform `observability.krateo.io` API group and rendered by the
portal — it *calls* an agent, it is not one.

## Controller

Each `Alert` is a provider-runtime managed resource whose external resource is its HyperDX alert
and dashboard. A reconcile is one pass, every `config.reconcileInterval`:

- Connect reads the HyperDX sources and ensures the shared webhook, at most once per interval for
  all Alerts. A recreated webhook resets every Alert, which rebuilds its HyperDX alert on it.
- Observe finds the HyperDX alert by `status.hyperdxAlertId`, else by name, mirrors its state and
  plans the push. It never reports the alert missing: provider-runtime's create handshake would
  then block an Alert whose create was cut short until a person clears an annotation. Update
  creates a missing alert, or pushes the spec.
- The Observes of one interval share one read of HyperDX's alerts list (`GET /api/v2/alerts`),
  not one per Alert, so a mirrored `state` is at most one interval old, the poll's own
  granularity. Every write the controller makes to the alerts or dashboards drops the list, so the
  next pass reads its own write.
- HyperDX rate-limits its API. A `429` is reported to provider-runtime's rate-limit governor,
  which holds every Alert's calls until the `Retry-After` HyperDX sent (a poll interval when it
  sent none, 10 minutes at most): the pass is deferred, with `Synced=False` reason
  `ReconcileDeferred` naming the time, and `status.phase` and `Ready` are left as they were. A
  `429` on an Update (a push or a create) is still that pass's error; the passes after it are
  deferred.
- A pass that finds the alert ALERT fires it, once per interval: reconciles the poll did not
  schedule (a spec edit, an error's retry) fire nothing.
- `krateo.io/paused` is also provider-runtime's annotation for skipping a whole reconcile. An
  Alert hides it from provider-runtime, so a paused Alert is still pushed and mirrored and only
  its firings stop.
- The finalizer `observability.krateo.io/hyperdx-cleanup` removes the HyperDX alert and dashboard
  before the Alert goes, best-effort, so a delete is never wedged on HyperDX.
- `status.conditions` carries provider-runtime's `Ready` and `Synced`, and the provider's
  `OpensIncidents` (see [Alert status](#alert-status)); `status.phase` keeps its meaning. The
  controller emits no warning Events: the seeded reconcile-error alert counts
  `CannotObserveExternalResource` Events, and a HyperDX outage would make it fire on itself.

## Alert sources

An Alert reads rows or a metric. A row Alert's `where` (and `value`) read the source
`spec.source` names: `logs`, the default when it is omitted, or `traces`, one row per span
(`ServiceName`, `SpanName`, `StatusCode`, `StatusMessage`, `Duration` in nanoseconds,
`SpanAttributes`). A traces Alert counts the spans `where` matches, e.g. `ServiceName =
'snowplow' AND StatusCode = 'Error'`, or aggregates a value read from them, e.g. the p95 of
`Duration / 1e6` in milliseconds. A metric Alert (`metricRef`) reads the metrics source.

- Each source is picked by its HyperDX kind (`log`, `trace`, `metric`), never by its position in
  HyperDX's list.
- Admission refuses `source` with `metricRef` (and so with `labels` and `ratio`, which need
  `metricRef`), and a traces Alert with an empty `where`: counting every span measures traffic,
  not a fault.
- An install without a traces source fails a traces Alert's pass with phase `Error`: the spec is
  valid, the install cannot evaluate it.
- Changing `source` with the same `where` rewrites the dashboard tile in place and keeps the
  HyperDX alert.

## Grouped alerts

A metric Alert with `spec.groupBy` is evaluated per series: one series per distinct tuple of the
keys' values, and HyperDX alerts on each on its own. Without it, an aggregation over every series
lets a healthy one mask an unhealthy one (`max` of a readiness gauge is 1 while one container is 0).

- Each key is read off the point, or off its resource when the point lacks it, as `labels` match:
  per key, the expression is `if(notEmpty(arrayElement(Attributes, 'k')),
  arrayElement(Attributes, 'k'), arrayElement(ResourceAttributes, 'k'))`. No `[` and no `=`:
  `/api/v2/charts/series` looks a group's value up by its column name, and ClickHouse names those
  columns `arrayElement(...)` and `equals(...)`, so either would come back null.
- The tile's `config.groupBy` carries each expression aliased as its key, `` <expr> AS `k` ``,
  comma-separated: backtick-quoted, so a key's dots are part of the name, and admission admits no
  backtick or backslash in a key. HyperDX's alert evaluation names a firing group after
  the result's column names, `<column>:<value>, …`, and renders the tile's groupBy raw into
  `SELECT` and `GROUP BY`, so the notification reads `k8s.namespace.name:<ns>,
  k8s.pod.name:<pod>, k8s.container.name:<c>` rather than repeating each expression.
- The `status.groups` query to `/api/v2/charts/series` sends the same expressions **unaliased**:
  charts/series takes `expr AS alias` (the SQL runs) but then looks each group's value up by the
  expression's text, so aliased it answers every group null. Probed on HyperDX 2.35, the aliased
  query collapsed 641 rows into 3. Both come from one per-key expression; only the tile adds the
  alias.
- The tile stays a line chart: the v2 API takes no groupBy on a number tile. Adding, changing or
  removing `groupBy` rewrites the tile in place and keeps the HyperDX alert. So does the upgrade
  from 0.2.64, whose tiles carry the expressions unaliased: one dashboard update, same alert.
- **What can be grouped.** HyperDX 2.35 renders a tile's groupBy as a plain select list only for a
  gauge (any aggregation) and for a sum with `avg`, `max`, `min`, `sum` or `p95`. Two other paths
  are refused until each is proven live (references are to `@hyperdx/api@2.35.0`,
  `packages/common-utils/src/core/`):
  - `aggregation: increase` with `groupBy` is refused at admission, and again by the reconciler.
    HyperDX pastes a grouped increase's groupBy, as one string, into its TopGroups ranking's
    `tuple(...)` and a `toString(...)` null filter (`renderChartConfig.ts:1949-2007`). HyperDX's
    own comment there calls an alias a syntax error, and several keys do not render. On a counter,
    `aggregation: sum` totals the same per-interval deltas (`sum(Rate)`,
    `renderChartConfig.ts:2017-2030`) without that ranking, and it groups.
  - `groupBy` on a histogram Metric is refused by the reconciler, with `phase: Invalid` and nothing
    pushed. Admission cannot see it, because the type is the Metric's. HyperDX packs a histogram's
    groups into one array column, `[groupBy] AS group` (`histogram.ts:191`), so the notification
    would name a single `group` column, and the alias would sit inside an array literal.
- Admission refuses `groupBy` without `metricRef`, with `ratio`, and with more than 5 keys, a
  duplicate key, or a key that is not an attribute name.
- Admission also refuses a key that is a column name of HyperDX's metric query. Examples are
  `Attributes`, `ResourceAttributes`, `ServiceName`, `MetricName`, `TimeUnix`, `Value`,
  `LastValue`, `AttributesHash`, `__hdx_time_bucket` and `group`. ClickHouse resolves an alias
  before a column of the same name, so the tile's alias would replace that column. A key named
  `Attributes` would even alias over the map its own expression reads.
  - The list is case-sensitive, as ClickHouse identifiers are: `value` and `values` are admitted.
  - It was derived from HyperDX 2.35's gauge, sum and histogram query paths, and each name is
    cited next to the rule in `crd.alert.yaml`.
- The notification names the group without help: HyperDX 2.35 opens a grouped tile alert's
  notification body with its own `Group: "<group>"` line (`{{#if group}}Group: "{{{group}}}"{{/if}}`),
  on a firing and a resolve alike, and the Alert's `spec.description` follows it as written,
  then a blank line and `spec.runbook` when there is one; the webhook log reads HyperDX's line.
- `spec.consecutiveWindows` (1 to 60, 1 when omitted) is pushed as HyperDX's
  `numConsecutiveWindows`, on any Alert; HyperDX counts it per group.

HyperDX's semantics, as of 2.35.0:

- A gauge's value in one interval is each series' last sample in it; `aggregation` then combines the
  series that share a group. With a key that identifies one series (a container), `max` and `min`
  agree.
- A group breaching for fewer than `consecutiveWindows` intervals is PENDING and notifies nothing;
  one interval within the threshold resolves it (and notifies, if it had fired).
- An interval with no data is skipped, and a skip restarts the count.
- The alert's state is a roll-up, ALERT while any group is. A series that stops reporting (its pod
  deleted) is never resolved: its last state stays. The v2 API exposes only the roll-up, so the
  provider computes `status.groups` itself (see [Alert status](#alert-status)).
- `status.groups` is judged one interval late: the newest completed interval may not be ingested
  yet, and judging it would drop a breaching series for a pass and list it again on the next. So
  `status.groups` may trail HyperDX's own state by one interval, on purpose.
- HyperDX judges an interval the moment it closes (about half a second after), with no delay for
  ingestion, and 2.35 has none to configure (`scheduleOffsetMinutes` only shifts the alignment and
  must be shorter than the interval). So **the metric must be sampled more often than the
  interval**, or the interval's only point is still in flight and HyperDX sees no data. With a
  `below` threshold that empty interval breaches (0 is below), and HyperDX fires one alert with no
  group (`group: ""`, no `Group:` line) every interval. Seen live: `k8s.container.ready` from
  `k8sclusterreceiver` at its 60 s default stamps every point at second :59, ingested a few seconds
  later, so a `1m` alert on it read nothing in every interval. At a 30 s collection interval each
  minute holds a mid-minute point by the time HyperDX looks. `status.groups` is unaffected, since it
  is judged one interval late.
- For the same reason, a `below` metric alert fires, ungrouped, whenever its metric stops arriving
  (a collector down): it reads as a breach, not as missing data.

## Alert identity

One `Alert` CR is one HyperDX alert and its own incidents, keyed on the CR's namespace and `metadata.name`.
`spec.displayName` is only a label and may repeat.

- The reconciler names each HyperDX alert after its CR (`HyperDXName`): its `metadata.name` in the
  home namespace, `<namespace>/<name>` in any other, on a single-tile dashboard
  `krateo-alert-<that name>` that no other alert evaluates. An alert claimed by several CRs, in
  any namespaces, stays with the CR it is named after; the others create their own.
- HyperDX's webhook template has no alert id. The shared webhook body is
  `{"alertName":"{{title}}","state":"{{state}}","body":"{{body}}","eventId":"{{eventId}}","source":"hyperdx-alert"}`,
  and the title is a state emoji plus the HyperDX alert name. `{{body}}` is the rendered
  notification and `{{eventId}}` a hash of the alert, channel and group; HyperDX escapes body and
  title into the JSON string. The reconciler updates an existing webhook whose body differs in
  place (PUT, same id), so an install converges without recreating any alert.
- The provider only logs a notification, one line, e.g. `[webhook] '🚨 sre-pod-not-ready' ALERT
  group="k8s.namespace.name:<ns>, k8s.pod.name:<pod>, k8s.container.name:<c>" eventId=<hash>: the
  reconciler evaluates it on its next pass` (each `<key>:<value>`, the key being the tile's alias). Of
  the body it logs only the group line of a grouped alert (`group=…`: HyperDX's quoted
  `Group: "…"` line, else the first unquoted `Group:` line), never the rest, which can quote the
  records an alert counted; an ungrouped alert's line has no `group=`.
- A firing is the CR the reconciler just mirrored, so it is never matched to an `Alert` by name.
- Its incidents carry the label `observability.krateo.io/alert: <metadata.name>` and
  `spec.alertRef {name, namespace}`. A name over 63 characters cannot be a label value, so such an
  `Alert` opens no incident.

## Namespaces

The provider reconciles the `Alert`s of every namespace, and reads the `Metric` a `metricRef`
names in the Alert's own namespace. A tenant's Alerts live in its namespace, created by the
platform, and Kubernetes RBAC decides who reads them: a tenant gets read-only access to the Alerts
of its own namespace (see [usage](usage.md#who-can-see-alerts)). Platform-wide Alerts stay in the
home namespace, the release namespace (`NAMESPACE`).

- **Names.** HyperDX has one namespace of names. A home Alert keeps its bare `metadata.name`, as
  every earlier release named it, so a live install's HyperDX alerts and dashboards are adopted as
  they are. Any other Alert is `<namespace>/<name>`: neither a namespace nor an object name holds
  `/`, so two namespaces' `llm-budget` are `team-a/llm-budget` and `team-b/llm-budget` and never
  a home Alert's name. The longest is 63 + 1 + 253 = 317 characters, under HyperDX's 512.
- **No incident outside home.** A firing of an Alert outside the home namespace mirrors `ALERT`
  on its status and notifies through the webhook, and that is all: no comparison, no Incident, no
  RCA. incident-agent reads the whole platform, so an analysis of a tenant's alert could quote other
  tenants' data. The incident writer refuses such a firing too, whatever hands it one. An Alert
  outside home with no `spec.description` gets `<namespace>/<name> threshold crossed.`, without the
  default's promise of a triage.
- **What stays home.** Incidents, the `hyperdx-api-token` Secret, the comparison's ModelConfig,
  the seeded default Alerts, the webhook and its Service.
- **Cache and RBAC.** The manager caches Alerts and Metrics in every namespace, everything else
  in the home one. The Role `krateo-alert-provider` keeps the home namespace's grants; the
  ClusterRole `krateo-alert-provider-alerts` reads and writes Alerts and their status, reads
  Metrics and writes Events everywhere, bound to the controller's own ServiceAccount.

## Incidents

An alert has an incident per problem, and several may be open at once; an incident is open in any
state but `Resolved` and `Closed`. For each firing, the incident writer (`internal/incident`):

1. lists the Incidents in the home namespace, the only one whose Alerts reach the writer (see [Namespaces](#namespaces)), labelled with the Alert's name;
2. asks the comparison model, in one call over the open incidents that have an analysis (a root
   cause, or a report from an RCA that did not fail) and are not stale, which one causes the
   alert's current records (see [Incident comparison](#incident-comparison)). That one covers the
   firing: no RCA, and no status write. The firing stamps its time in the incident's
   `observability.krateo.io/last-covered-at` annotation, at most once per 1/24 of
   `config.incidentCoverWindow`, so an incident whose alert keeps firing is not rewritten every
   pass. An incident is stale when it neither opened nor covered a firing within
   `config.incidentCoverWindow` (24h): an incident nobody moves on would otherwise take every later
   firing of its alert, and a new problem would get no RCA. A stale incident is left as it is;
   when the firing opens a new incident (step 6), that one names the stale ones, newest first and
   at most five, in `observability.krateo.io/supersedes-stale`;
3. else the newest open incident without an analysis covers the firing: one still
   `Analyzing`, or one whose RCA failed (whatever text the failure left in `status.report`) less
   than `config.failedAnalysisHold` (1800 s) after its `status.completedAt`; else the newest
   incident closed as a failed analysis (`observability.krateo.io/closed-reason: analysis-failed`),
   not yet superseded, inside the same hold. There is nothing to compare with, and a new incident
   would rerun the analysis, which while the agent or its model is down fails the same way. Past
   the hold, a failed incident takes no more firings, so the next firing nothing else covers opens
   a new incident with a fresh RCA, which supersedes it (step 6);
4. else, if the alert's latest incident is `Resolved` and its `status.resolution.at` is less than
   one `spec.interval` ago (5m when unset), that incident covers the firing the same way, and it
   stays `Resolved`. A `where` alert keeps counting the rows from before the fix for its lookback
   window, and those firings belong to the incident the fix resolved. A `Closed` incident gets no
   such window: after a human close, the next firing opens a new one;
5. else, when the comparison gave no verdict, records nothing: whether the firing is a new problem
   is unknown, and the next pass asks again;
6. otherwise creates `<alert>-<yyyymmdd-hhmmss>` (the firing's UTC time) with the label and
   `spec.alertRef`, `trigger: alert`, `prompt` and `triggeredAt`, in state `Analyzing`. The
   prompt names the alert, its `where` and its `spec.description` as the intent. An Alert with a
   `spec.runbook` adds it, quoted between `<runbook>` tags under the prompt's own label: operator
   guidance to read before proposing a fix, not instructions that change the agent's method or
   the answer's format. Without one the prompt is unchanged. The new incident supersedes the
   alert's failed analyses not yet superseded, newest first and at most five: it names them in
   `observability.krateo.io/supersedes-failed`, each gets `observability.krateo.io/superseded-by`
   naming it, and one still `Open` without scripts (from before failed analyses were closed, or
   whose close was refused) is closed (`spec.closed`, with the closed-reason annotation). A
   person's incident is never superseded;
7. runs the RCA on the incident's own kagent thread (contextId = uuid5 of its name), checks its
   scripts (see [Script checks](#script-checks)), then writes the analysis, `howToFix` and
   `state: Open` in one status write.

- One evaluation per alert runs at a time: while one is still comparing, the alert's next firing
  is skipped. The RCA runs after that evaluation ends, and its incident takes the firings that
  arrive meanwhile (step 3).
- At most `config.maxConcurrentAnalyses` RCAs (2) run at once, so alerts that fire together do not
  spend the model's per-minute token quota at once and fail every RCA; a new incident waits in
  `Analyzing` for a slot.
- An RCA that failed transiently (the call failed, the answer was empty, or it was only
  kagent's `LLM error:`) runs again `config.analysisRetries` (2) times, `config.analysisRetryDelay`
  (120 s) after the first failure and twice as long after each next one, each on a kagent thread
  of its own. The incident stays `Analyzing` meanwhile.
- An answer without scripts that can run (no `json` block, a `howToFix` the parser drops, or
  scripts the [script checks](#script-checks) refuse) goes back to the agent once, on the same
  kagent thread, with what is wrong with it, and the corrected answer is checked the same way. A
  `json` block without any `howToFix` is the agent's choice and is not sent back.
- An RCA that still fails, or still has no scripts that can run, is a failed analysis. The
  incident is closed, never left `Open` with nothing to check: one object patch sets
  `spec.closed: true` and the annotation `observability.krateo.io/closed-reason: analysis-failed`,
  then the analysis is written without a state, with `status.error` saying why there are no
  scripts. Both fields go in that one patch, which is the contract: the provider never writes
  `resolution`, and incident-provider turns `spec.closed` into `Closed`. From the release carrying
  incident-provider#19 it records `resolution.by: analysis-failed` when one patch from one field
  manager set both; older versions record `resolution.by: user` (see [api](api.md)). A failed analysis
  keeps covering its alert's firings for `config.failedAnalysisHold` (step 3), then the next
  incident supersedes it (step 6). Should the close patch be refused, the analysis is written
  `Open` with the error, and that next incident closes it.
- The one exception is an automatic reanalysis (`status.reanalysis.cause: ApplyActionStale`,
  filed by incident-provider on an `Open` incident) that fails, however it fails. An automatic
  action never ends an open incident, so it goes back to `Open`, not closed, with
  `status.reanalysis` kept and `status.error` saying why: `The automatic reanalysis failed: <why>`
  when it got no answer (its RCA failed transiently through all its retries, or a restart
  interrupted it past its recovery), `The automatic reanalysis produced no usable fix: <why>`
  when the answer had no scripts that can run, even after the correction turn. A person can ask
  for a reanalysis again. A person's reanalysis that fails is closed as above.
- An incident a human closed or moved on while it was analyzing keeps its state (`Closed` is
  final): the analysis is written without a state, and a failed one is not closed again.
- At startup the provider runs again the RCA of every incident a restart left `Analyzing`, off
  the startup path, on a thread of its own (`<name>#recovery-<n>`), with its `spec.prompt` (and a
  reanalysis' feedback), counted in `observability.krateo.io/analysis-recoveries`. An incident
  interrupted again (one recovery already), or without a prompt, is closed as a failed analysis
  with the reason in `error`, except an automatic reanalysis, which goes back to `Open` with it
  (above). A person's incident whose first write never landed is opened with
  it. An incident closed in spec is left to incident-provider.
- The alert returning to OK changes nothing. From `Open` on, the incident controller runs its
  scripts and moves it, or a human closes it.

## Script checks

Before the writer stores a `howToFix` (`internal/scriptcheck`), every script must pass four
checks, each a failure seen on a live cluster:

| Rule | Scripts | Refuses |
|---|---|---|
| bash syntax | all four | a script bash cannot parse, checked with mvdan.cc/sh's bash parser, in process: the image is distroless, without bash, and nothing the model wrote runs here |
| jq syntax | all four | a jq program, passed literally to `jq`, that does not parse (github.com/itchyny/gojq's parser), such as `"\-"`: jq exits 3 and the check never answers. Only the parse is checked: gojq's builtins differ from jq's |
| the Alert CR | precondition, verify | an unguarded read of `alerts.observability.krateo.io` (singular, with its version, as `type/name`, in a `--raw` path or assigned to a variable), or of `alert`/`alerts` where kubectl takes a kind. The checks test the root-cause object, which is the Alert only for a false positive of its own query; unguarded, a read of a deleted Alert answers unknown (exit 2) forever. See the guarded form below |
| `.core` suffix | all four | a core kind with a group suffix (`nodes.core`, `pods.v1.core`) where kubectl takes a kind: kubectl rejects it |

A guarded Alert read is the one shape the checker accepts, and it refuses any other:

- a top-level statement `v=$(kubectl get <kind> [<name>] ... --ignore-not-found)`, optionally
  followed by `|| exit <code>`. The substitution holds that one kubectl call, with no pipe; its verb
  is `get`; its kind argument names one kind, not a comma list; and `--ignore-not-found` is set
  (bare or `=true`);
- and the very next top-level statement exits 0 when `v` is empty: `[[ -n $v ]] || exit 0`,
  `[[ -z $v ]] && exit 0`, the same with `[ ]` or `test`, or `if [[ -z $v ]]; then exit 0; fi`
  (no `else`), with `$v`, `${v}` or either one double-quoted.

A read inside a function, a loop or a condition, through a variable or a `--raw` path, piped, or
with any statement between the read and the test, is refused. A deleted Alert then exits 0, and
incident-provider closes the incident (`AlertGone`).

The extraction is conservative: a word that is not literal in the source (a variable, a command
substitution) is not guessed at, so only what is certainly wrong is refused. A refused precondition,
apply or verify drops the whole `howToFix`; a refused rollback alone is left out, with a
`missingContext` line, as the parser leaves out an unusable one. A script known to be broken is
never written.

## Alerts a person analyses

An Alert labelled `observability.krateo.io/analysis: human` is for records that must not reach a
model, such as a token in telemetry. Its firing opens an incident as usual, with the label copied
to it, but no comparison, no RCA and no reanalysis run, and its records are not read. The incident
starts `Open`, with `status.error` saying why it has no scripts. The newest open incident takes
every later firing without a write, until a person closes it. The next firing after that opens a
new one, past the `Resolved` grace window. A person is the only reader of the matched row.

## Reanalysis

A new analysis of an `Open` or `Verifying` incident is requested by setting `spec.reanalysis`,
optionally with `feedback`, and with a `cause`:

- `Person` (also when `cause` is absent): the portal's Re-run analysis; `feedback` is what the
  person found wrong.
- `ApplyActionStale`: incident-provider files the request itself when the cluster stops serving
  the version its `howToFix.applyAction` targets. alert-provider has no trigger of its own.

A cause outside those two is taken as `Person`, and logged. The provider watches Incidents for the
request, and:

1. clears the analysis, `howToFix`, `lastChecks`, `verifyingSince` and the `Reproduced`
   condition, records the request in `status.reanalysis` (`at`, `feedback`, `count`, `cause`) and sets
   `Analyzing`, in one status write conditioned on the incident's resourceVersion;
2. removes `spec.reanalysis`, only after step 1 landed: incident-provider files an
   `ApplyActionStale` request only while no consumed request covers the stale episode, judged by
   `status.reanalysis.at`, so removing the request first could have it filed again;
3. runs the RCA on a thread of its own (`<name>#reanalysis-<count>`), with the original prompt
   plus the previous root cause or error, each previous script with its last result (and, for an
   unknown result, what the run printed), the previous applyAction, and the feedback; then writes
   it as in step 7 above.

After the previous scripts, a previous `howToFix.applyAction` is its own section, titled exactly
`Previous applyAction:`, with a ```` ```json ```` block of one line: `verb`, `apiVersion`,
`resource`, `namespace`, `name`, `payload` in that order (those present), each value rendered as
Python's `json.dumps` (`pyfmt.Dumps`: `", "` and `": "` separators, non-ASCII escaped, nested keys
sorted). When the incident's `ApplyActionStale` condition (set by incident-provider) has status
`True`, the next paragraph is `The previous applyAction can no longer be sent: <message>.`, with the
condition's message verbatim. Without an applyAction neither appears, and the prompt is as before.

The prompt's reanalysis section opens with its cause. For `Person`: "A person asked for a new
analysis of this incident. The previous one is below; it may be wrong.", and the feedback is
titled `What the person found wrong with the previous analysis:`. For `ApplyActionStale`: "The
previous fix can no longer be applied: its applyAction targets a version this cluster no longer
serves. Re-resolve the fix against the served version. The previous analysis is below.", and the
feedback is titled `Why the analysis is requested again:`. A reanalysis a restart interrupted
reruns with the cause recorded in `status.reanalysis`.

A request on an incident in any other state is removed unanswered: an `Analyzing` incident is
already being analyzed, and a `Resolved` or `Closed` one is over. So is a request on an incident
labelled `observability.krateo.io/analysis: human`, which a person analyzes. incident-provider
mirrors this rule (`writerAnswers`) to file `ApplyActionStale` requests only where they are
answered; a change to it here goes there too.

A failed automatic reanalysis left `Open` without scripts is, like any such incident, a failed
analysis to its alert: if the alert fires again past `config.failedAnalysisHold` and nothing else
covers the firing, the new incident, with a fresh RCA, supersedes it (`superseded-by`) and closes
it. That is the normal lifecycle of a new firing, not the automatic reanalysis closing anything.

## Incident comparison

`internal/compare` holds both sides of it: the prompt and the verdict parser.

- One chat completion per comparison, with no agent and no tools, on the model of the kagent
  ModelConfig `config.compareModelConfig` (default `gemini-flash`): provider OpenAI at its
  `baseUrl` (behind the agentgateway, its `/llm/v1` route, keyed by the service JWT under
  `apiKeyPassthrough`), or provider Gemini with its `apiKeySecret`. It times out after
  `COMPARE_TIMEOUT` (60 s).
- The system prompt is two sentences. The user message holds the alert, its current records and
  the `config.maxCompareCandidates` (50) newest open analyzed incidents, newest first, each as its root cause and its
  precondition, apply and verify scripts. It leaves out `spec.runbook`: whether an incident's
  root cause produces the records is decided by the records and the incidents, and guidance on
  how to respond says nothing about which incident caused them.
- The records are the alert's `where` rows over one `spec.interval`, from HyperDX's
  `/api/v2/charts/series` grouped by `compare.RowGroup`: a line per distinct record (a k8s
  event's object, reason and message; any other log's service and pod), or for a traces Alert by
  `compare.TraceRowGroup` (a span's service, namespace, name, status and status message), with its
  count, the 20 most frequent quoted, with anything shaped like a JWT redacted, as the collectors
  do. No row is no verdict: HyperDX's `ALERT` comes from its last
  evaluation, and the window read later can have aged past every row.
- The answer is `{"match": <incident number> | null, "reason": "…"}`: the newest incident whose
  root cause produces any of the records, or null for none. The prompt numbers the incidents
  rather than naming them, because the gateway's PhoneNumber guard masks the timestamp in a name.
  A failed or timed-out call, a 429, or an answer whose `match` is not null or a candidate's
  number is no verdict.
- A firing alert costs at most one call per pass, whatever the number of its open incidents.

## Alert status

The reconciler mirrors the HyperDX alert's `state` onto `status.state` every cycle, and writes
`status.okSince` in the same patch: the time the alert last turned OK, kept while it stays OK and
unset in any other state. It is for display ("OK for 13 min") and never closes anything.

`status.stateSince` is the transition time of `status.state`, whatever the state: written when a
pass reads a different state than the one last written (OK ↔ ALERT, → PENDING, → empty), and kept
on every re-read of the same one. So "ALERT since 09:12" is when this firing began, while
`status.stateObservedAt` is only the last read. For an OK alert it equals `okSince`. An Alert that
was already ALERT or PENDING before the provider wrote the field has it empty until its next
transition: the provider never saw that state begin and does not claim the upgrade time. Display
only, like `okSince`.

Every pass also writes what the Alert evaluates and whether its firings open incidents, both
derived from the spec, the pause annotation and the namespace, so a reader of the CR needs no rule
to tell them apart. `kubectl get alerts` shows both, as the `Type` and `Incidents` columns.

- `status.type` is `Logs` or `Traces` for an Alert that counts log rows or spans, `Metric` for one
  that aggregates a metric, and `LogValue` or `TraceValue` for one that aggregates a value read
  from log rows or spans.
- The `OpensIncidents` condition is `True` (reason `CountsRecords`) when a firing opens an
  incident, else `False` with the first reason that holds: `OutsideHomeNamespace` (see
  [Namespaces](#namespaces)), `AggregatedValue` (a metric or a value has no records to compare and
  root-cause), `Paused` (`krateo.io/paused`). The reasons a person cannot lift come first, so a
  paused metric Alert reads `AggregatedValue`: resuming it would open no incident either. Its
  message says why in a sentence.
- The firing gate reads the same condition, so the status and what the provider does never
  disagree. An unchanged condition keeps its `lastTransitionTime`.

A grouped Alert's `status.groups` lists its series that are ALERT, computed by the provider each
pass, since HyperDX's API carries only the roll-up and never resolves a series that is gone. The
pass queries `/api/v2/charts/series` with the tile's select and groupBy, at the Alert's interval,
over `consecutiveWindows` completed intervals aligned as HyperDX aligns its buckets, judged one
interval late: the window ends at the start of the newest completed interval, which may not be
ingested yet (with `1m` and 5, at 12:07:30 it reads 12:01 to 12:05 and leaves 12:06 out). The
list may therefore trail HyperDX's state by one interval, on purpose, and never flaps on a minute
still being ingested. A series is listed only when each of those intervals has a value
that breaches the threshold, compared as HyperDX's `doesExceedThreshold` compares; a missing
interval is no breach, as in HyperDX. Each entry is `{group: {<key>: <value>}, value: <last
interval's value>, since: <RFC 3339>}`, `since` kept from the previous pass for a series already
listed and otherwise the start of the window. A series that recovers or disappears drops out. The
list is sorted by group and holds at most 50 entries; `status.groupsTruncated` counts the rest. A
failed query leaves `status.groups` as it was and is the pass's error (in the `Synced` condition;
an HTTP error leaves `status.phase`, any other sets `Error`); a pass with a push reads the groups
after pushing, so a failed read never holds a push back.
