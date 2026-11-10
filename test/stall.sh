#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
# Every $ in a single-quoted jq program below is jq's own ($ENV, $limit,
# $checks), so it must not expand; SC2016 warns about exactly that.
# shellcheck disable=SC2016
set -euo pipefail

CHECK="${1:?which check: open, stuck, renovate or slots}"

# Finds the ways the update machine stops without anything failing loudly.
# Each check prints what it found and exits 1 if it found anything, so
# .github/workflows/stall.yml goes red and GitHub emails someone.
AUTHOR=app/divbast-renovate

found() {
    echo "${1}"
    echo "${2}"
    exit 1
}

# A Renovate pull request open too long: an update that never goes green.
open() {
    local query list
    query='.[] | select(now - (.createdAt | fromdateiso8601) > ($ENV.OPEN_DAYS | tonumber) * 86400)
        | "#\(.number) \(.title)"'
    list=$(gh pr list --author "${AUTHOR}" --state open --limit 100 \
        --json number,title,createdAt --jq "${query}")
    [[ -z "${list}" ]] || found "Open more than ${OPEN_DAYS:?} days:" "${list}"
    echo "no Renovate pull request open more than ${OPEN_DAYS} days"
}

# Red for longer than the limit since the pull request opened, or pending
# for longer than it since the last push. Red counts from the opening,
# because every rebase re-runs CI and would otherwise reset the clock. A
# check still running, or CI that never started (no `lint` check at all),
# counts from the last push. Renovate waits on all of these for ever.
stuck() {
    local query list
    query='((($ENV.STUCK_HOURS | tonumber) * 3600) as $limit
        | .[]
        | .statusCheckRollup as $checks
        | [$checks[] | select(.__typename == "CheckRun") | .name] as $names
        | select(
            (now - (.createdAt | fromdateiso8601) > $limit
             and ([$checks[] | select(
                    (.__typename == "CheckRun" and .conclusion != null
                     and (.conclusion | IN("SUCCESS", "SKIPPED", "NEUTRAL") | not))
                    or (.__typename == "StatusContext" and (.state | IN("FAILURE", "ERROR"))))] | length > 0))
            or (now - (.commits[-1].committedDate | fromdateiso8601) > $limit
                and (([$checks[] | select(
                        (.__typename == "CheckRun" and .conclusion == null)
                        or (.__typename == "StatusContext" and (.state | IN("PENDING", "EXPECTED"))))] | length > 0)
                     or ("lint" | IN($names[]) | not))))
        | "#\(.number) \(.title)")'
    list=$(gh pr list --author "${AUTHOR}" --state open --limit 40 \
        --json number,title,createdAt,commits,statusCheckRollup --jq "${query}")
    [[ -z "${list}" ]] || found "Not green for more than ${STUCK_HOURS:?} hours:" "${list}"
    echo "no Renovate pull request red or pending more than ${STUCK_HOURS} hours"
}

# No successful Renovate run within the limit. A run its `if` skipped ends
# as skipped, not success, so the App's own dashboard edits do not count.
renovate() {
    local query last
    query='if length == 0 then "no successful Renovate run"
        elif now - (.[0].createdAt | fromdateiso8601) > ($ENV.RUN_HOURS | tonumber) * 3600
        then "last successful Renovate run \(.[0].createdAt)"
        else empty end'
    last=$(gh run list --workflow renovate.yml --status success --limit 1 --json createdAt --jq "${query}")
    [[ -z "${last}" ]] || found "Renovate silent for more than ${RUN_HOURS:?} hours:" "${last}"
    echo "Renovate succeeded within ${RUN_HOURS} hours"
}

# Daily slots that should have published and did not. A finished slot run
# that built a commit ahead of the published one would have published it,
# so while :latest still names an older commit, each such run is a lost
# slot, whether it failed, was cancelled or skipped. So is each day with no
# slot run at all, after 12 hours' grace: GitHub starts them hours late. One
# lost slot, an upstream outage say, already emails on its own as a red run.
slots() {
    local revision main runs completed sha url status lost=0 missed list=""
    # PUBLISHED_REVISION stands in for the registry in tests only.
    if [[ -n "${PUBLISHED_REVISION:-}" ]]; then
        revision="${PUBLISHED_REVISION}"
    else
        revision=$(test/published-revision.sh "${PUBLISHED:?}")
    fi
    main=$(gh api "repos/${GH_REPO:?}/commits/main" --jq .sha)
    if [[ "${revision}" == "${main}" ]]; then
        echo "main ${main} is published"
        return 0
    fi

    runs=$(gh run list --workflow build.yml --event schedule --branch main --limit 10 \
        --json headSha,createdAt,status,url)
    completed=$(jq -r '.[] | select(.status == "completed") | "\(.headSha) \(.url)"' <<<"${runs}")
    while read -r sha url; do
        [[ -n "${sha}" ]] || continue
        status=ahead
        if [[ -n "${revision}" ]]; then
            status=$(gh api "repos/${GH_REPO}/compare/${revision}...${sha}" --jq .status)
        fi
        if [[ "${status}" == ahead ]]; then
            lost=$((lost + 1))
            list+="  lost slot: ${url} built ${sha}"$'\n'
        fi
    done <<<"${completed}"

    missed=$(jq --argjson none "${LOST_SLOTS:?}" '
        if length == 0 then $none
        else ((now - (.[0].createdAt | fromdateiso8601) - 43200) / 86400 | floor | [., 0] | max) end' <<<"${runs}")
    if (( missed > 0 )); then
        list+="  ${missed} day(s) with no slot run since $(jq -r '.[0].createdAt // "ever"' <<<"${runs}")"$'\n'
    fi

    if (( lost + missed >= LOST_SLOTS )); then
        found "main ${main} is unpublished, :latest is ${revision:-absent}, and ${LOST_SLOTS} or more slots were lost:" "${list%$'\n'}"
    fi
    echo "main ${main} is ahead of :latest ${revision:-(absent)}, with $((lost + missed)) lost slot(s), under ${LOST_SLOTS}"
}

case "${CHECK}" in
    open | stuck | renovate | slots) "${CHECK}" ;;
    *) echo "no such check: ${CHECK}" >&2; exit 1 ;;
esac
