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
| `config.autopilotA2aUrl` | `http://autopilot.krateo-system.svc:8080/` | the RCA agent's A2A endpoint: autopilot, which delegates to incident-agent; through the gateway, the ledger also reads the sub-agents' sessions |
| `config.compareModelConfig` | `gemini-flash` | the kagent ModelConfig whose model names the open incident that covers a firing; provider OpenAI or Gemini |
| `config.maxCompareCandidates` | `50` | how many of an alert's newest open, analyzed incidents one comparison weighs |
| `config.failedAnalysisHold` | `1800` | seconds an incident whose RCA failed keeps taking its alert's firings; past it, a new incident gets a fresh RCA |
| `config.analysisRetries` | `2` | how many more times an RCA that failed transiently runs |
| `config.analysisRetryDelay` | `120` | seconds before the first RCA retry; each next one waits twice as long |
| `config.authnUrl` | `""` | authn, for the service JWT on both A2A calls; empty = unauthenticated |
| `config.reconcileInterval` | `60` | seconds between reconciler passes; each pass that finds an alert ALERT is one firing |

Runtime env (set on the Deployment) carries these, plus `A2A_TIMEOUT` (2700 s, the longest silence on the RCA stream) and
`COMPARE_TIMEOUT` (60 s, one comparison). See `helm/alert-provider/templates/deployment.yaml`.
