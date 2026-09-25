#!/usr/bin/env bash
#
# Before/after for PerfectScale on the five demo tenants.
#
#   perfectscale.sh before   switch automation off, put every tenant back on
#                            its original Deployment spec, and prove it
#   perfectscale.sh after    switch automation on, then time how long
#                            PerfectScale takes to act on each tenant
#   perfectscale.sh status   template vs running pod, per tenant, right now
#
# Why "before" deletes pods instead of running `kubectl rollout restart`:
# PerfectScale resizes pods in place and leaves the Deployment untouched, so
# the template still holds the original numbers and a fresh pod picks them up.
# A rollout restart would get the same numbers, but it stamps the template,
# which creates a new ReplicaSet and a new revision. PerfectScale is
# revision-aware and treats a new revision as new code with no history, so
# the timing measured by "after" would include waiting for data on it.
# Deleting the pods keeps the same revision, which is what a fair "how long
# until it acts" measurement needs.
#
# And why automation has to be off first: with it on, PerfectScale's admission
# webhook puts its current recommendation straight onto the replacement pod,
# so "before" would never appear at all.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TENANTS=(team-analytics team-billing team-payments team-scratch team-search)
CFG=tenant-automation
K=(kubectl --context "${KCTX_ALIAS}")

need jq "brew install jq"

deploy_of() { "${K[@]}" get deploy -n "$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }

# Resources as sorted, compact JSON, so template and pod compare as strings.
template_res() {
  "${K[@]}" get deploy -n "$1" "$2" -o json 2>/dev/null \
    | jq -cS '.spec.template.spec.containers[0].resources // {}'
}
pod_res() {
  "${K[@]}" get pods -n "$1" -l "app=$2" -o json 2>/dev/null \
    | jq -cS '[.items[] | select(.metadata.deletionTimestamp == null)][0].spec.containers[0].resources // "none"'
}

# One line, human-readable: cpu/mem request, then limit if there is one.
fmt() {
  jq -r 'if type == "string" then . else
    ((.requests.cpu // "-") + "/" + (.requests.memory // "-")) +
    (if .limits then "  lim " + ((.limits.cpu // "-") + "/" + (.limits.memory // "-")) else "" end)
  end' <<<"$1"
}

set_mode() {
  local mode=$1 ns
  for ns in "${TENANTS[@]}"; do
    "${K[@]}" patch namespaceautomationconfig "$CFG" -n "$ns" --type merge -p "{
      \"spec\": {\"automation\": {
        \"operational\": {\"automationMode\": \"$mode\"},
        \"workloadTypes\": {\"Deployment\": {\"operational\": {\"automationMode\": \"$mode\"}}}
      }}}" >/dev/null || die "could not set $ns to $mode (is 20-tenants.yaml applied? make eks-ps-apply)"
  done
  ok "automation ${mode} for ${#TENANTS[@]} tenants"
}

elapsed() { local s=$(( $(date +%s) - $1 )); printf '%dm%02ds' $((s / 60)) $((s % 60)); }

cmd_status() {
  printf '\n  %-15s %-8s %-34s %s\n' TENANT MODE "ORIGINAL (template)" "RUNNING POD"
  local ns d mode t p mark
  for ns in "${TENANTS[@]}"; do
    d=$(deploy_of "$ns")
    mode=$("${K[@]}" get namespaceautomationconfig "$CFG" -n "$ns" -o jsonpath='{.spec.automation.workloadTypes.Deployment.operational.automationMode}' 2>/dev/null)
    t=$(template_res "$ns" "$d"); p=$(pod_res "$ns" "$d")
    mark="${C_GRN}original${C_OFF}"; [[ "$t" != "$p" ]] && mark="${C_YEL}resized${C_OFF}"
    printf '  %-15s %-8s %-34s %s  %s\n' "$ns" "${mode:-?}" "$(fmt "$t")" "$(fmt "$p")" "$mark"
  done
  echo
}

is_original() {
  local d; d=$(deploy_of "$1")
  [[ "$(template_res "$1" "$d")" == "$(pod_res "$1" "$d")" ]]
}

replace_pods() {
  local ns d
  for ns in "$@"; do
    d=$(deploy_of "$ns")
    "${K[@]}" delete pods -n "$ns" -l "app=$d" --wait=false >/dev/null && info "$ns/$d"
  done
  # search never becomes Ready (it OOMs by design), so wait for a live
  # replacement pod rather than for readiness.
  local start pending=1; start=$(date +%s)
  while (( pending )); do
    pending=0
    for ns in "$@"; do
      [[ "$(pod_res "$ns" "$(deploy_of "$ns")")" == '"none"' ]] && pending=1
    done
    if (( $(date +%s) - start > 180 )); then warn "some pods not back after 3m"; break; fi
    # if, not &&: lib.sh sets -e, and a trailing "(( pending )) && sleep" that
    # is false becomes the function's exit status, which ends the script.
    if (( pending )); then sleep 5; fi
  done
  return 0
}

cmd_before() {
  step "Switching automation off"
  set_mode Disabled

  # Check the outcome rather than waiting on a status: disabling a workload
  # leaves its last automation status in place, so the status is not a
  # reliable "stood down" signal. Replace the pods, check them, and replace
  # again any that were still resized on the way in.
  info "giving the agent 30s to pick up the change"
  sleep 30

  local attempt todo=("${TENANTS[@]}") left ns
  for attempt in 1 2 3; do
    step "Replacing pods so they start from the original spec (attempt $attempt)"
    replace_pods "${todo[@]}"
    left=()
    for ns in "${todo[@]}"; do is_original "$ns" || left+=("$ns"); done
    if (( ${#left[@]} == 0 )); then
      ok "every tenant is on its original spec"
      break
    fi
    warn "still resized on the way in: ${left[*]}; retrying in 30s"
    todo=("${left[@]}")
    sleep 30
  done

  cmd_status
  if (( ${#left[@]} )); then
    warn "not reset: ${left[*]}. PerfectScale is still resizing them; check the tenant CRs."
    return 1
  fi
  info "Now: make eks-ps-after"
}

cmd_after() {
  local timeout=${PS_TIMEOUT:-1800}
  step "Switching automation on"
  local t0; t0=$(date +%s)
  set_mode Enabled
  info "started $(date -u +%H:%M:%SZ); watching every 10s for up to $((timeout / 60))m (PS_TIMEOUT to change)"

  # Indexed arrays parallel to TENANTS: macOS ships bash 3.2, which has no
  # associative arrays, and this has to run on the laptop it is presented from.
  local i n=${#TENANTS[@]} acted=() orig=() count=0
  for ((i = 0; i < n; i++)); do
    orig[i]=$(template_res "${TENANTS[i]}" "$(deploy_of "${TENANTS[i]}")")
    acted[i]=""
  done

  step "Waiting for PerfectScale to act"
  while (( count < n )) && (( $(date +%s) - t0 < timeout )); do
    for ((i = 0; i < n; i++)); do
      [[ -n "${acted[i]}" ]] && continue
      local ns=${TENANTS[i]} p
      p=$(pod_res "$ns" "$(deploy_of "$ns")")
      if [[ "$p" != '"none"' && "$p" != "${orig[i]}" ]]; then
        acted[i]=$(elapsed "$t0"); count=$((count + 1))
        ok "$(printf '%-15s acted after %-7s %s  ->  %s' "$ns" "${acted[i]}" "$(fmt "${orig[i]}")" "$(fmt "$p")")"
      fi
    done
    if (( count < n )); then sleep 10; fi
  done

  step "Summary"
  for ((i = 0; i < n; i++)); do
    if [[ -n "${acted[i]}" ]]; then info "$(printf '%-15s %s' "${TENANTS[i]}" "${acted[i]}")"
    else info "$(printf '%-15s no change in %dm' "${TENANTS[i]}" $((timeout / 60)))"; fi
  done
  (( count < n )) && info "A tenant with nothing to fix may never change; that is expected."
  return 0
}

case "${1:-status}" in
  before) cmd_before ;;
  after)  cmd_after ;;
  status) cmd_status ;;
  *) die "usage: $0 before|after|status" ;;
esac
