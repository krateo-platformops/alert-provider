#!/usr/bin/env bash
# Holds while the latest served version of installers.composition.krateo.io defines gatewayPathsFrom without accessRequestIntake in its enum.
crd=$(kubectl get customresourcedefinitions.apiextensions.krateo.io installers.composition.krateo.io -o json --ignore-not-found) || exit 2
[[ -n $crd ]] || exit 0
has_served=$(jq -r 'any(.spec.versions[]; .served == true)' <<<"$crd") || exit 2
[[ $has_served == true ]] || exit 2
jq -e --arg v accessRequestIntake '
  ([.spec.versions[] | select(.served == true)] | last | .schema.openAPIV3Schema.properties.spec.properties.components.items.properties.gatewayPathsFrom // null) as $g |
  ($g != null) and (($g.enum // []) | index($v) == null)
' <<<"$crd" >/dev/null
case $? in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac

