#!/usr/bin/env bash
# Verified once NightlyReview is off 0.2.2 and ready, CronJob nightly-review has succeeded on schedule, and nightly-review-agent is ready.
nr=$(kubectl get nightlyreviews.composition.krateo.io nightly-review -n krateo-system -o json --ignore-not-found) || exit 2
[[ -n $nr ]] || exit 0
jq -e --arg v 0.2.2 --arg t True --arg r Ready '
  ((.status.helmChartVersion // "") != $v)
  and ((.status.observedGeneration // 0) >= .metadata.generation)
  and any(.status.conditions[]?; .type == $r and .status == $t)
' <<<"$nr" >/dev/null
case $? in 0) ;; 1) exit 1 ;; *) exit 2 ;; esac
cj=$(kubectl get cronjobs.batch nightly-review -n krateo-system -o json --ignore-not-found) || exit 2
[[ -n $cj ]] || exit 0
jq -e --arg old ghcr.io/krateo-platformops/nightly-review:0.2.2 '
  all(.spec.jobTemplate.spec.template.spec.containers[]; .image != $old)
  and ((.status.lastSuccessfulTime // "") != "")
  and ((.status.lastSuccessfulTime // "") >= (.status.lastScheduleTime // ""))
' <<<"$cj" >/dev/null
case $? in 0) ;; 1) exit 1 ;; *) exit 2 ;; esac
d=$(kubectl get deployments.apps nightly-review-agent -n krateo-system -o json --ignore-not-found) || exit 2
[[ -n $d ]] || exit 0
jq -e '
  ((.status.observedGeneration // 0) >= .metadata.generation)
  and ((.status.updatedReplicas // 0) == (.spec.replicas // 1))
  and ((.status.availableReplicas // 0) == (.spec.replicas // 1))
' <<<"$d" >/dev/null
case $? in 0) exit 0 ;; 1) exit 1 ;; *) exit 2 ;; esac

