#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -uo pipefail
cd "$(dirname "${0}")" || exit 1
# shellcheck source=test/lib.sh
source ./lib.sh

ENGINE="${ENGINE:-docker}"
IMAGE="${IMAGE:-localhost/librespeed:dev}"
PORT=18190
BASE="http://127.0.0.1:${PORT}"
CTR=librespeed-acc
INTERNAL_CTR=librespeed-acc-internal
NET=librespeed-acc-net
INTERNAL_NET=librespeed-acc-internal-net

CONFINE=(--cap-drop ALL --read-only --user 65532:65532 --tmpfs /tmp)

PHP_BIN=/usr/local/bin/frankenphp
PROBE_SRC=./in-container.php

# The probe lands on /tmp, the one tmpfs the contract grants
PROBE=/tmp/in-container.php

# Ship the probe into a container. `cp` refuses a container whose root is
# read-only, and with no volume there is nowhere else it could write, so the
# probe goes in on stdin and the container's own PHP writes it out. When that
# fails, the engine's own words are the only clue to why, so they are printed
# rather than dropped.
install_probe() {
    local ctr="${1}" out
    if ! out=$("${ENGINE}" exec -i "${ctr}" "${PHP_BIN}" php-cli -r \
            "exit(file_put_contents(\$argv[1], stream_get_contents(STDIN)) === false ? 1 : 0);" \
            "${PROBE}" <"${PROBE_SRC}" 2>&1); then
        fail "could not install ${PROBE_SRC} at ${ctr}:${PROBE}"
        printf '       | %s exec: %s\n' "${ENGINE}" "${out}" >&2
    fi
}

# Run one probe subcommand and fold record it prints into pass/fail
probe() {
    local ctr="${1}"
    shift
    local subcommand="${1}"
    local out rc line verdict what expected found saw=0

    out="$("${ENGINE}" exec "${ctr}" "${PHP_BIN}" php-cli "${PROBE}" "${@}" 2>&1)"
    rc=${?}

    while IFS= read -r line; do
        IFS=$'\t' read -r verdict what expected found <<<"${line}"
        case "${verdict}" in
            ok)
                saw=1
                pass "${what} (${found})"
                ;;
            FAIL)
                saw=1
                fail "${what}: expected '${expected}', got '${found}'"
                ;;
            '') ;;
            *) printf '       | %s\n' "${line}" >&2 ;;
        esac
    done <<<"${out}"

    if (( saw == 0 )); then
        fail "${ctr}: probe '${subcommand}' produced no assertions (exit ${rc})"
    fi

    return "${rc}"
}

cleanup() {
    "${ENGINE}" rm -f "${CTR}" "${INTERNAL_CTR}" >/dev/null 2>&1 || true
    "${ENGINE}" network rm -f "${NET}" "${INTERNAL_NET}" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

"${ENGINE}" network create "${NET}" >/dev/null

echo '== 1. runs under the exact contract, with no volume'
"${ENGINE}" run -d --name "${CTR}" "${CONFINE[@]}" \
    --network "${NET}" -p "${PORT}:8080" \
    "${IMAGE}" >/dev/null
install_probe "${CTR}"
if wait_for_port "${BASE}/backend/getIP.php"; then
    pass 'container serves'
else
    fail 'container never served'
    "${ENGINE}" logs "${CTR}"
fi
assert_status 200 '/backend/getIP.php' "${BASE}/backend/getIP.php"

echo '== 2. confinement'
# The raw /proc/self/status line carries the labels CapEff/CapPrm
probe "${CTR}" assert-caps
# Under --cap-drop ALL the kernel refuses to exec a binary that carries a file
# capability, with EPERM, before a line of PHP runs. Upstream's own FrankenPHP
# builds set cap_net_bind_service=ep.
out=$("${ENGINE}" run --rm "${CONFINE[@]}" --network "${NET}" \
    --entrypoint "${PHP_BIN}" "${IMAGE}" version 2>&1)
rc=$?
assert_eq 0 "${rc}" "${PHP_BIN} runs under --cap-drop ALL, so it carries no file capability"
(( rc == 0 )) || printf '       | %s\n' "${out}" >&2
probe "${CTR}" assert-read-only
# What a user who passes no --user gets, which --user above cannot stand in for
if user=$("${ENGINE}" image inspect --format '{{.Config.User}}' "${IMAGE}" 2>&1); then
    assert_eq 65532:65532 "${user}" 'the image runs as uid/gid 65532 by default'
else
    fail "the image runs as uid/gid 65532 by default: ${ENGINE} image inspect failed: ${user}"
fi
# pcntl_exec() overlays the bootstrap process with the server
probe "${CTR}" assert-pid1
probe "${CTR}" assert-no-shell

echo '== 3. no egress, measured from a genuinely --internal network'
"${ENGINE}" network create --internal "${INTERNAL_NET}" >/dev/null
"${ENGINE}" run -d --name "${INTERNAL_CTR}" "${CONFINE[@]}" \
    --network "${INTERNAL_NET}" \
    "${IMAGE}" >/dev/null
install_probe "${INTERNAL_CTR}"

if internal=$("${ENGINE}" network inspect "${INTERNAL_NET}" --format '{{.Internal}}'); then
    assert_eq true "${internal}" 'test network really is internal'
else
    fail "test network really is internal: ${ENGINE} network inspect ${INTERNAL_NET} failed"
fi

probe "${INTERNAL_CTR}" assert-pid1 || "${ENGINE}" logs "${INTERNAL_CTR}"
probe "${INTERNAL_CTR}" probe-egress

"${ENGINE}" rm -f "${INTERNAL_CTR}" >/dev/null 2>&1
"${ENGINE}" network rm -f "${INTERNAL_NET}" >/dev/null 2>&1

echo '== 4. PHP refuses URL streams and keeps errors off the page'
if ini=$("${ENGINE}" exec "${CTR}" "${PHP_BIN}" php-cli "${PROBE}" php-ini 2>&1) \
        && jq -e 'type == "object"' >/dev/null 2>&1 <<<"${ini}"; then
    allow_url_fopen=$(jq -r '.allow_url_fopen' <<<"${ini}")
    display_errors=$(jq -r '.display_errors' <<<"${ini}")
    assert_eq false "${allow_url_fopen}" 'allow_url_fopen'
    assert_eq false "${display_errors}" 'display_errors'
else
    fail "php-ini printed no JSON object: ${ini}"
fi

echo
if (( FAILURES == 0 )); then
    echo "acceptance: all checks passed"
    exit 0
fi
echo "acceptance: ${FAILURES} check(s) failed" >&2
exit 1
