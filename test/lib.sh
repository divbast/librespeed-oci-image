# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
# shellcheck shell=bash

FAILURES=0

pass() { printf '  ok   %s\n' "${1}"; }
fail() { printf '  FAIL %s\n' "${1}" >&2; FAILURES=$((FAILURES + 1)); }

assert_eq() {
    local expected="${1}" actual="${2}" what="${3}"
    if [[ "${actual}" == "${expected}" ]]; then
        pass "${what} (${actual})"
    else
        fail "${what}: expected '${expected}', got '${actual}'"
    fi
}

# Deliberately no --fail and no 2xx matching. Every Baikal failure mode returns
# 200 with an exception page, so only an exact status code proves anything.
http_status() {
    curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --max-time 5 \
        -X "${2:-GET}" "${1}"
}

assert_status() {
    local expected="${1}" what="${2}"
    shift 2
    local actual rc
    actual=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 30 "${@}")
    rc=$?

    if (( rc != 0 )) && [[ "${actual}" != "000" ]]; then
        fail "${what}: curl exited ${rc} after receiving ${actual}, so the request never completed"
        return
    fi

    assert_eq "${expected}" "${actual}" "${what}"
}

assert_contains() {
    local needle="${1}" what="${2}"
    shift 2
    local output rc
    output=$("${@}" 2>&1)
    rc=$?

    if (( rc != 0 )); then
        fail "${what}: ${1} exited ${rc}"
        printf '%s\n' "${output}" >&2
        return
    fi

    if [[ "${output}" == *"${needle}"* ]]; then
        pass "${what}"
    else
        fail "${what}: output contains no '${needle}'"
        printf '%s\n' "${output}" >&2
    fi
}

WAIT_FOR_PORT_SECONDS=60

wait_for_port() {
    local url="${1}" status deadline=$(( SECONDS + WAIT_FOR_PORT_SECONDS ))
    while (( SECONDS < deadline )); do
        status=$(http_status "${url}")
        if [[ "${status}" != "000" ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}
