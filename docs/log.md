---
type: Log
title: Log
description: Notable changes.
tags: [observability, alerts]
timestamp: 2026-08-20T00:00:00Z
---

# Log

## 2026-08-20
- Moved `krateo-agentiko` -> `krateo-platformops` (observability plumbing, not an agent) and standardized: wrapped the raw manifests in a helm chart, bare-semver + `CHART_VERSION`, owner-derived image build (was pinned at `krateo-agentiko` `v0.2.17`), public security caller, preflight, OKF docs.

## 2026-09-28
- Renamed alert-troubleshooter to **alert-provider**: charts `alert-provider` and `alert-provider-crds`, image `ghcr.io/krateo-platformops/alert-provider`, Deployment/Service/ServiceAccount `krateo-alert-provider`, authn identity `krateo-alert-provider` (group `krateo:alert-provider`), HyperDX webhook `krateo-alert-provider`. The GitHub repo keeps its name.
- An alert has an incident per problem: every reconciler pass that finds it ALERT is a firing, which counts on the open incident autopilot judges the same problem, or opens a new one. The HyperDX webhook only logs.
- `Alert.spec.apiRef` and `status.value` are gone; `howToFix` gains an optional `rollback`.
- `howToFix` gains an optional `applyAction`, the single Kubernetes write the portal's Apply button sends as the clicking user.
- A failed RCA's error text in `status.report` no longer makes its incident comparable, so it takes the alert's firings uncompared instead of opening a new incident each pass. At most 2 RCAs run at once (`MAX_CONCURRENT_ANALYSES`).
- The chart takes `imagePullSecrets` for its private image, set on the `krateo-alert-provider` ServiceAccount.

## 2026-09-30
- A firing is compared with all its alert's open incidents in one chat completion on the model of `config.compareModelConfig` (default `gemini-flash`), with a two-sentence system prompt, the alert's current records from HyperDX, and each incident's root cause and scripts. `config.compareA2aUrl` is gone.

## 2026-10-01
- A comparison weighs the 50 newest open analyzed incidents, set by `config.maxCompareCandidates`.
- An incident whose RCA failed takes its alert's firings for `config.failedAnalysisHold` (1800 s) after the failure, then no more: the next firing nothing else covers opens a new incident with a fresh RCA. An interrupted RCA stamps `completedAt` too.

## 2026-10-05
- The seeded `krateo-composition-reconcile-error` excludes `incident-controller`: its own "Reconciler error" lines are about Incidents, and on krateo-057 they were ~21k rows a day that kept the alert firing every minute. Seeding only creates an absent Alert, so an existing one keeps its old `where` until it is edited or deleted and reseeded.
- An Alert whose spec push succeeds again clears the previous `SpecDrift` `error`.
- An `applyAction` may name a Krateo composition's dashed version (`composition.krateo.io/v1-12-36`); the parser used to drop it, so a fix that patches a composition's spec had no Apply button.

## 2026-10-06
- The provider is a Go controller on Krateo's provider-runtime, in place of the Python reconcile loop; the chart's values and environment, the Alert and Incident contracts and the webhook are unchanged. `Alert.status.conditions` carries provider-runtime's `Ready` and `Synced`. The bootstrap Job runs `/bin/alert-provider bootstrap`. The Role grants `events.k8s.io` Events.
- The GitHub repository is `krateo-platformops/alert-provider`; GitHub redirects the old `alert-troubleshooter` name.

## 2026-10-09
- A firing an incident covers writes nothing: `status.firings` and `status.lastFiredAt` are gone from the Incident, so an incident whose alert keeps firing is no longer rewritten every pass. The Alert's `status.lastSyncedAt` is gone too: an Alert whose HyperDX state does not change is not rewritten.

## 2026-10-19
- The seeded alerts skip DELETED watch records: the collector writes one when the apiserver expires an Event, an hour after it, repeating its last state, so each failure fired the alert a second time. Seeding only creates an absent Alert, so an existing one keeps its old `where` until it is edited or deleted and reseeded.
- A firing keeps the alert's last answered comparison while its candidate incidents are the same and every record it matches was among the ones that comparison saw; only a new record or a changed incident calls the model. A failed call is not kept, so the next firing retries it. Replayed over 8 days of krateo-057's comparisons, it skips 84% of the calls.

## 2026-10-21
- An RCA that failed transiently (the call failed, the answer was empty, or it was only kagent's `LLM error:`) runs again, 2 more times by default, 2 then 4 minutes later (`config.analysisRetries`, `config.analysisRetryDelay`). The incident stays `Analyzing` meanwhile.
- A person can ask for a new analysis of an `Open` or `Verifying` incident with `spec.reanalysis`, optionally with feedback. The provider clears the old analysis and its check results, records the request in `status.reanalysis`, and runs the RCA again with the previous analysis, its scripts' last results and the feedback in the prompt. The Role gains `watch` on incidents.

## 2026-10-31
- Alerts are reconciled in every namespace: a tenant's Alerts live in its namespace, where RBAC decides who reads them. Outside the release namespace an Alert is named `<namespace>/<name>` in HyperDX (home Alerts keep their bare names, so live HyperDX objects are adopted unchanged) and mirrors its state, but its firing opens no Incident and runs no comparison or RCA: incident-agent reads the whole platform. The incident writer refuses such a firing too. The chart adds the ClusterRole `krateo-alert-provider-alerts` (Alerts, their status, Metrics, Events) for the controller's ServiceAccount; the Role keeps the release namespace's grants.

## 2026-10-22
- `A2A_TIMEOUT` defaults to 900 s. The RCA stream is silent while the agent waits on a model call, and the agentgateway LLM route now waits up to 600 s for one; at 180 s a slow provider failed the RCA and every retry.
- The RCA prompt's check rules: full resource names in every script (`deployments.apps`, not `deployment`), `any`/`all` for yes/no jq tests, text into jq with `--arg`, and a precondition whose object is gone exits 0. Its examples follow them.

## 2026-11-09
- The Alerts' passes of one poll interval share one read of HyperDX's alerts list instead of one each, and any write the controller makes to the alerts or dashboards drops it. A mirrored `state` is at most one interval old.
- A HyperDX `429` defers the pass instead of failing it: provider-runtime's rate-limit governor holds every Alert's calls until the `Retry-After` (a poll interval without one, 10 minutes at most), with `Synced=False/ReconcileDeferred`; `status.phase` is left as it was.
- A log record written inside traced work carries the `trace_id` and `span_id` of the span it ran in (provider-runtime v1.13.0); lines outside any span carry neither (see configuration, Tracing).
- An open incident covers its alert's firings only while it is fresh: it opened, or last covered a firing, within `config.incidentCoverWindow` (default `24h`, a Go duration; `"0"` disables it). A stale one is no longer offered to the comparison, so a firing it would have covered opens a new incident with a fresh RCA; the new incident names the stale ones it skipped (at most five) in the `observability.krateo.io/supersedes-stale` annotation, and they are left as they are. A covering firing stamps `observability.krateo.io/last-covered-at` on its incident (a metadata merge patch, at most once per 1/24 of the window), so a problem that keeps firing stays covered. `incident.fire` carries `incident.stale_skipped`.
- A failed analysis is closed, never left `Open` with nothing to check: after the transient retries and one correction turn, an analysis without scripts that can run sets `spec.closed: true` and `observability.krateo.io/closed-reason: analysis-failed`, with `status.error` saying why. It still covers its alert's firings for `config.failedAnalysisHold`; the alert's next incident then supersedes it (`observability.krateo.io/supersedes-failed` and `superseded-by`) and closes any failed analysis still `Open` from before. On krateo-057, 35 such incidents had piled up `Open`.
- An answer without scripts that can run goes back to the agent once, on the same kagent thread, with what is wrong with it; `incident.analyze` carries `incident.analysis.corrections`.
- At startup an incident a restart left `Analyzing` reruns its RCA once (`<name>#recovery-1`, counted in `observability.krateo.io/analysis-recoveries`) instead of opening with an error; interrupted again, it is closed as a failed analysis.
- Scripts are checked before they are written (`internal/scriptcheck`): bash syntax (mvdan.cc/sh, no exec), every literal jq program parses (gojq), a precondition or verify reads the Alert CR only guarded (`--ignore-not-found` into a variable, then exit 0 on an empty result, so a deleted Alert never answers 2), and no kind carries a `.core` suffix. The RCA prompt states these rules, and its Alert false-positive example uses the guarded form.

## 2026-11-10
- provider-runtime v1.14.0 spans every Alert pass: its `reconcile` root, with `connect`, `observe`, `update` and `delete` children, replaces `alert.reconcile` and the `alert.observe`/`create`/`update`/`delete` spans. Their attributes (`alert.*`, `hyperdx.alerts_list.cached`, `hyperdx.throttled`) are on provider-runtime's spans; the creating pass is `update` with `alert.action=create`. `incident.fire` links to `observe`. The `incident.*` names are unchanged. The provider hands its redacting pipeline to provider-runtime, so the new spans are redacted the same way.
- The span resource carries `k8s.deployment.name` and `service.instance.id` (`DEPLOYMENT_NAME`, which the chart sets to `krateo-alert-provider` with tracing on); `OTEL_RESOURCE_ATTRIBUTES` wins.
- With tracing on, an Incident carries the trace to incident-provider: `krateo.io/traceparent` from `incident.fire` at create, and a fresh one from `incident.analyze` in a metadata patch just before the write that sets `Open` with the scripts (and before a reanalysis' `Open`), so the pass that starts the check Pod is a child of the analysis.
- An Alert that carries `krateo.io/traceparent` gets provider-runtime's `krateo.io/traceparent-continued` marker, with tracing on.
- provider-runtime v1.14.1 (design-tracing D8): every Alert `reconcile` is a new root. On an Alert stamped with `krateo.io/traceparent` it links to the stamp, and no pass is the stamp's child any more, so a chart upgrade no longer pulls every Alert's observe-only polls into its trace for 10 minutes. Only the first `update` or `delete` after a new value joins the stamper's trace, with the `k8s.object.*` and `alert.*` attributes and a link to its `reconcile`, which links back. The Incident stamps are unchanged: from v1.14.1 on, incident-provider's `update` that starts the check Pod is the child of `incident.analyze`, and its reconciles link to it. The span names are unchanged.

## 2026-11-11
- provider-runtime v1.15.0 (provider-runtime#74): the `reconcile` root of every Alert pass that starts an `update` or `delete` (the creating pass included, and a joined act too) carries `provider_runtime.acted=true`, whether it succeeds or not; an observe-only pass carries no such attribute. observability's tail sampler keeps those traces. An act that joins a stamp now links to its `reconcile` only when that root is head-sampled, the same rule as the root's link to it; under the default `parentbased_always_on` nothing changes. plumbing moves v1.13.0 → v1.48.0 with it; the `env`, `kubeutil/event` and `kubeutil/eventrecorder` packages the provider imports are unchanged (a gofmt-only whitespace change in `eventrecorder`).
- The RCA output contract asks for `rootCause.owner` (`customer`, `krateo`, `unknown`) and `rootCause.ownerEvidence` (at most 8 `{kind, ref, note}`), and constrains `rootCause.category` to incident-provider 0.10.0's enum, `permission` included. The parser fits all three to that CRD before the write (category to `other`, an unusable owner to `unknown`, bad evidence dropped, notes cut to 300 characters; no owner leaves owner and ownerEvidence out, not assessed) and logs what it coerced. Roll out after incident-provider 0.10.0: an older CRD prunes `owner` and `ownerEvidence` harmlessly, but a 0.10.0 CRD refuses any category outside its enum, which only this release coerces.

## 2026-11-12
- A reanalysis prompt carries the previous `howToFix.applyAction` after the previous scripts, in a section titled `Previous applyAction:` (a one-line `json` block, fields in the contract's order), and, when the `ApplyActionStale` condition is `True`, the line `The previous applyAction can no longer be sent: <condition message>.` A prompt without an applyAction is unchanged.
- A reanalysis request carries a `cause`: `Person` (the portal's, also when absent) or `ApplyActionStale` (filed by incident-provider when the cluster stops serving the applyAction's version). The provider records it in `status.reanalysis.cause`, and an `ApplyActionStale` prompt opens with "The previous fix can no longer be applied: ..." instead of "A person asked for a new analysis". The provider files no request itself.
- An automatic reanalysis (`cause: ApplyActionStale`) that fails goes back to `Open` with `status.reanalysis` kept, instead of being closed as a failed analysis: an automatic action never ends an open incident. `status.error` is `The automatic reanalysis failed: …` when it got no answer (transient failures through its retries, or a restart past its recovery), `The automatic reanalysis produced no usable fix: …` when the answer had no scripts that can run. A person's reanalysis that fails is closed as before. A later firing past the failed-analysis hold still supersedes such an incident, as any failed analysis.
- The chart exposes `config.maxConcurrentAnalyses` (`MAX_CONCURRENT_ANALYSES`, default 2) beside `config.analysisRetries` (2) and `config.analysisRetryDelay` (120 s).
- provider-runtime v1.15.0 → v1.17.1. The pass after a finalized delete, whose Get finds the Alert gone, no longer logs "Cannot get managed resource" at ERROR (provider-runtime#89, v1.17.1: Debug, no error, no requeue); it carried no trace id, inflated error counts and could trip log-based alerts. Nothing to adapt from v1.16.0 (a status conflict's error names its cause; no test here compares it, nothing uses `resource.IsAPIError`) or v1.17.0 (a new test helper): the Alert controller's methods never write the managed Alert, which `reconcilertest.AssertLeavesManagedCurrent` now pins. Plumbing stays at v1.48.0.
- Every Alert's status says what it evaluates and whether its firings open incidents, so a reader of the CR needs no rule to tell. `status.type` is `Logs`, `Traces`, `Metric`, `LogValue` or `TraceValue`. The `OpensIncidents` condition is `True` (reason `CountsRecords`), or `False` with the first reason that holds: `OutsideHomeNamespace`, `AggregatedValue`, `Paused`, with a one-sentence message. The firing gate reads the same condition, so the two never disagree; a paused metric Alert now logs "fires on an aggregated value" instead of "is paused". `kubectl get alerts` shows them as the `Type` and `Incidents` columns. Roll out the `alert-provider-crds` chart first: an older CRD prunes `status.type`, harmlessly.
- The reanalysis controller watches Incidents as metadata only. It cached every Incident whole as an unstructured map, and an Incident carries its analysis prompt (about 20 KB): demo's 1957 Incidents (1953 Closed, 37.8 MB of JSON) OOMKilled the controller on startup under its 256Mi limit, 65 restarts from 2026-11-11 19:06 UTC, so no Alert was evaluated there. It now passes every Incident the cache first lists (a request filed while it was down) and every spec write (`metadata.generation` changes; a reanalysis request is a spec write) to the writer, which reads the Incident past the cache as before; status writes pass nothing. Each start reads every Incident once, one at a time. Proven by an envtest test (`make test-integration`, new in `make check`): 500 Incidents of 20 KB grow the heap by ~1.3 MiB, against ~12 MiB cached whole.
- Every Alert carries `status.stateObservedAt`, stamped on every pass that reads the live HyperDX state, and `kubectl get alerts` shows it as the `Observed` column. `Ready` says how the last read went, so a provider that stops (demo, 2026-11-11: OOMKilled for a day) left every Alert `Ready=True`; only this stamp grows old. Roll out the `alert-provider-crds` chart first: an older CRD prunes the field, harmlessly.
- The writer lists Incidents page by page (100 a call) and drops each one's `spec.prompt` before keeping it; it reads a single Incident whole (Get) when it needs the prompt. 0.2.80's metadata-only watch fixed the steady state, but demo still OOMKilled once on 0.2.80's first start: the startup recovery (`RecoverInterrupted`) listed all 1957 Incidents whole in one call and decoded each into a second copy, and every firing listed its alert's Incidents whole (up to 510 on demo). Recovery now reads the whole Incident only for those it resumes. Proven on envtest: 250 Incidents of 20 KB list complete, with no prompt, in 3 paged calls.

## 2026-11-14
- Every Alert carries `status.stateSince`, the transition time of `status.state`: written when a pass reads a different state, kept on every re-read of the same one, so a reader can tell "ALERT for 3 minutes" from "ALERT for 3 days" (`stateObservedAt` is only the last read). An OK Alert's `stateSince` equals its `okSince`; one already ALERT or PENDING before the upgrade has it empty until its next transition, since that start was never observed. Asked for by the portal's /alerts overview.
