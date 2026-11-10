#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

EVENT="${1:?event that started the run: schedule or workflow_dispatch}"
BUILT="${2:?commit this run built}"
PUBLISHED="${3?commit the published image was built from, empty if none}"

# Decides whether this run publishes what it built. The daily slot publishes
# when main moved since the published image, and the button publishes even
# when it has not. Neither ever moves :latest backwards: a run that built an
# older commit would take back whatever the newer image fixed.
# A refusal goes to the run's summary page as well as the log, where a skip
# or a publish already says why, so a red slot says why in the same place.
fail() {
    echo "${1}" >&2
    if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
        echo "fail: ${1}" >>"${GITHUB_STEP_SUMMARY}"
    fi
    exit 1
}

decide() {
    echo "${1}: ${2}"
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
        if [[ "${1}" == publish ]]; then
            echo 'publish=true' >>"${GITHUB_OUTPUT}"
        else
            echo 'publish=false' >>"${GITHUB_OUTPUT}"
        fi
    fi
    if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
        echo "${1}: ${2}" >>"${GITHUB_STEP_SUMMARY}"
    fi
    exit 0
}

case "${EVENT}" in
    schedule | workflow_dispatch) ;;
    *) fail "only the daily slot and a manual run publish, not a ${EVENT} run" ;;
esac

if [[ -z "${PUBLISHED}" ]]; then
    decide publish "nothing is published yet"
fi

if ! git cat-file -e "${PUBLISHED}^{commit}" 2>/dev/null \
        || ! git merge-base --is-ancestor "${PUBLISHED}" origin/main; then
    fail "the published image was built from ${PUBLISHED}, which is not on main"
fi

if [[ "${PUBLISHED}" == "${BUILT}" ]]; then
    if [[ "${EVENT}" == schedule ]]; then
        decide skip "main has not moved since ${PUBLISHED}"
    fi
    decide publish "republishing ${PUBLISHED} by hand"
fi

if git merge-base --is-ancestor "${PUBLISHED}" "${BUILT}"; then
    decide publish "main moved from ${PUBLISHED} to ${BUILT}"
fi

if git merge-base --is-ancestor "${BUILT}" "${PUBLISHED}"; then
    decide skip "${PUBLISHED}, published while this run built ${BUILT}, is newer"
fi

fail "${BUILT} and the published ${PUBLISHED} have diverged"
