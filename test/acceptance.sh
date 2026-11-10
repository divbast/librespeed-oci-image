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
HOSTILE_PORT=18191
HOSTILE_BASE="http://127.0.0.1:${HOSTILE_PORT}"
CTR=librespeed-acc
INTERNAL_CTR=librespeed-acc-internal
HOSTILE_CTR=librespeed-acc-hostile
BADENV_CTR=librespeed-acc-badenv
NET=librespeed-acc-net
INTERNAL_NET=librespeed-acc-internal-net

# The size the README states. Every container here gets exactly this, so no
# check passes on more room than the contract grants.
TMPFS_SIZE=16m
CONFINE=(--cap-drop ALL --read-only --user 65532:65532 --tmpfs "/tmp:size=${TMPFS_SIZE}")

PHP_BIN=/usr/local/bin/frankenphp
PROBE_SRC=./in-container.php

# The probe lands on /tmp, the one tmpfs the contract grants
PROBE=/tmp/in-container.php

# What getIP.php answers a client on loopback
GETIP_LOOPBACK='{"processedString":"127.0.0.1","rawIspInfo":""}'

# The speed test's upload: speedtest_worker.js posts blobs of
# xhr_ul_blob_megabytes, 20 by default, each of 1048576 bytes
UPLOAD_BYTES=$(( 20 * 1048576 ))
WORK=$(mktemp -d) || exit 1
UPLOAD_BODY="${WORK}/upload.bin"
head -c "${UPLOAD_BYTES}" /dev/zero >"${UPLOAD_BODY}" || exit 1

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

# probe-getip, asked again each second while the server is still starting.
# Prints the body, or the last failure.
getip_body() {
    local ctr="${1}" out deadline=$(( SECONDS + WAIT_FOR_PORT_SECONDS ))
    while :; do
        if out=$("${ENGINE}" exec "${ctr}" "${PHP_BIN}" php-cli "${PROBE}" probe-getip 2>&1); then
            printf '%s' "${out}"
            return 0
        fi
        if (( SECONDS >= deadline )); then
            printf '%s' "${out}"
            return 1
        fi
        sleep 1
    done
}

# assert_contains's opposite. Output that is empty would lack anything, so it
# fails rather than passes.
assert_lacks() {
    local needle="${1}" what="${2}"
    shift 2
    local output rc
    output=$("${@}" 2>&1)
    rc=$?

    if (( rc != 0 )); then
        fail "${what}: ${1} exited ${rc}"
        printf '%s\n' "${output}" >&2
    elif [[ -z "${output}" ]]; then
        fail "${what}: ${1} printed nothing to look through"
    elif [[ "${output}" == *"${needle}"* ]]; then
        fail "${what}: output contains '${needle}'"
    else
        pass "${what}"
    fi
}

# POST the upload body to empty.php, then print "<curl exit> <status> <bytes
# sent>". Browsers send no Expect: 100-continue, and neither does this, though
# curl would add one itself to a body over 1 MiB.
post_upload() {
    local tag="${1}"
    shift
    local out rc
    out=$(curl -s -o /dev/null -w '%{http_code} %{size_upload}' --connect-timeout 5 --max-time 60 \
        -H 'Expect:' "${@}" --data-binary "@${UPLOAD_BODY}" "${BASE}/backend/empty.php?acc=${tag}")
    rc=$?
    printf '%s %s\n' "${rc}" "${out}"
}

# The bytes the server read of one request's body, from :8080's access log,
# which may land a moment after the response does
bytes_read() {
    local ctr="${1}" uri="${2}" got deadline=$(( SECONDS + 5 ))
    while :; do
        got=$("${ENGINE}" logs "${ctr}" 2>/dev/null \
            | jq -Rr --arg uri "${uri}" 'fromjson? | select(.request.uri? == $uri) | .bytes_read')
        if [[ -n "${got}" ]] || (( SECONDS >= deadline )); then
            printf '%s' "${got:-no access log line for ${uri}}"
            return
        fi
        sleep 1
    done
}

# An upload the server stops reading half way still ends in a 200 and curl
# exit 0, so only the byte counts on both sides show it arrived whole
check_upload() {
    local tag="${1}" what="${2}" rc status sent got
    read -r rc status sent <<<"${3}"
    assert_eq "0 200 ${UPLOAD_BYTES}" "${rc} ${status} ${sent}" "${what}: curl exit, status, bytes sent"
    got=$(bytes_read "${CTR}" "/backend/empty.php?acc=${tag}")
    assert_eq "${UPLOAD_BYTES}" "${got}" "${what}: bytes the server read"
}

# A dot segment in the path picks the route by the path it resolves to, and
# then FrankenPHP picks the script to run. That must be the script the route
# allows: getIP.php's own answer, or a refusal. A helper such as
# getIP_util.php answers 200 with an empty body instead.
DOTSEG_CLIENT=203.0.113.9
assert_getip_or_refused() {
    local path="${1}" out status body
    out=$(curl -s --path-as-is --max-time 5 -w '\n%{http_code}' \
        -H "X-Forwarded-For: ${DOTSEG_CLIENT}" "${BASE}${path}")
    status=${out##*$'\n'}
    body=${out%$'\n'*}
    case "${status}" in
        200)
            assert_eq "{\"processedString\":\"${DOTSEG_CLIENT}\",\"rawIspInfo\":\"\"}" "${body}" \
                "GET ${path} runs getIP.php"
            ;;
        400 | 404) pass "GET ${path} is refused (${status})" ;;
        *) fail "GET ${path}: expected getIP.php's answer or a refusal, got ${status}" ;;
    esac
}

# Print a container's exit code once it has stopped, or "running" if it is
# still up after the given number of seconds
exit_code_within() {
    local ctr="${1}" deadline=$(( SECONDS + ${2} )) state
    while :; do
        if ! state=$("${ENGINE}" inspect --format '{{.State.Running}} {{.State.ExitCode}}' "${ctr}" 2>&1); then
            printf 'unknown (%s)' "${state}"
            return
        fi
        if [[ "${state}" == "false "* ]]; then
            printf '%s' "${state#false }"
            return
        fi
        if (( SECONDS >= deadline )); then
            printf 'running'
            return
        fi
        sleep 1
    done
}

cleanup() {
    "${ENGINE}" rm -f "${CTR}" "${INTERNAL_CTR}" "${HOSTILE_CTR}" "${BADENV_CTR}" >/dev/null 2>&1 || true
    "${ENGINE}" network rm -f "${NET}" "${INTERNAL_NET}" >/dev/null 2>&1 || true
}
trap 'cleanup; rm -rf "${WORK}"' EXIT
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
# Egress refused proves something only of a server that works where it is
if body=$(getip_body "${INTERNAL_CTR}"); then
    assert_eq "${GETIP_LOOPBACK}" "${body}" 'serves GET /backend/getIP.php inside the internal network'
else
    fail "serves GET /backend/getIP.php inside the internal network: ${body}"
fi
probe "${INTERNAL_CTR}" probe-egress

"${ENGINE}" rm -f "${INTERNAL_CTR}" >/dev/null 2>&1
"${ENGINE}" network rm -f "${INTERNAL_NET}" >/dev/null 2>&1

echo '== 4. PHP refuses URL streams, keeps errors off the page and bodies off /tmp'
if ini=$("${ENGINE}" exec "${CTR}" "${PHP_BIN}" php-cli "${PROBE}" php-ini 2>&1) \
        && jq -e 'type == "object"' >/dev/null 2>&1 <<<"${ini}"; then
    allow_url_fopen=$(jq -r '.allow_url_fopen' <<<"${ini}")
    display_errors=$(jq -r '.display_errors' <<<"${ini}")
    post_max_size=$(jq -r '.post_max_size_bytes' <<<"${ini}")
    assert_eq false "${allow_url_fopen}" 'allow_url_fopen'
    assert_eq false "${display_errors}" 'display_errors'
    # PHP keeps a body it reads in memory up to 16 KiB and writes the rest
    # to /tmp. It reads none bigger than post_max_size, unless that is 0,
    # which lifts the limit altogether.
    if [[ "${post_max_size}" =~ ^[0-9]+$ ]] && (( post_max_size > 0 && post_max_size < 16384 )); then
        pass "post_max_size is under PHP's 16 KiB in-memory buffer (${post_max_size} bytes)"
    else
        fail "post_max_size: expected 1 to 16383 bytes, got '${post_max_size}'"
    fi
else
    fail "php-ini printed no JSON object: ${ini}"
fi
assert_lacks 'X-Powered-By' 'responses do not name PHP and its version' \
    curl -s -D - -o /dev/null --max-time 5 "${BASE}/backend/getIP.php"

echo '== 5. only the modern UI, its generated config and the speed test are served'
for path in / /settings.json /server-list.json /servers.json /speedtest.js /speedtest_worker.js \
        /stability.html /stability_worker.js /frontend/javascript/index.js; do
    assert_status 200 "GET ${path}" "${BASE}${path}"
done
assert_contains 'id="privacy-warning"' 'GET / is the modern UI' curl -s --max-time 5 "${BASE}/"
assert_lacks 'index-classic' 'GET / does not lead to the classic UI' curl -s --max-time 5 "${BASE}/"
# Unset, TITLE, TAGLINE and GDPR_EMAIL leave upstream's own text
assert_contains '<h1>Free and Open Source Speedtest.</h1>' "GET / keeps upstream's heading" \
    curl -s --max-time 5 "${BASE}/"
assert_contains '<p class="tagline">No Flash, No Java, No Websockets, No Bullsh*t</p>' \
    "GET / keeps upstream's tagline" curl -s --max-time 5 "${BASE}/"
for path in /index-classic.html /config.json /design-switch.js /examples/example-singleServer-basic.html \
        /results/sanitycheck.php /backend/getIP_util.php /backend/getIP_ipInfo_apikey.php \
        /backend/geoip2.phar /backend/country_asn.mmdb /docker/entrypoint.sh /frontend/index.html \
        /healthz; do
    assert_status 404 "GET ${path}" "${BASE}${path}"
done
# Allowed prefixes must not reach past themselves: the routes match the path
# the file server would open, not the one the client wrote
for path in /frontend/../backend/geoip2.phar /frontend/%2e%2e/backend/geoip2.phar \
        /images/..%2fbackend/country_asn.mmdb; do
    assert_status 404 "GET ${path}" --path-as-is "${BASE}${path}"
done
# Nor may a path that resolves onto an allowed route run some other script
for path in /backend/getIP_util.php/../getIP.php /backend/getIP_util.php/%2e%2e/getIP.php \
        /backend/getIP_util.php/%2E%2E/getIP.php /backend/getIP_util.php/.%2e/getIP.php \
        /backend/getIP_ipInfo_apikey.php/../getIP.php /results/telemetry.php/../../backend/getIP.php \
        /results/telemetry.php/%2e%2e/%2e%2e/backend/getIP.php /results/stats.php/../../backend/getIP.php \
        /results/json.php/%2e%2e/%2e%2e/backend/getIP.php; do
    assert_getip_or_refused "${path}"
done
# or reach, from an allowed one, a script the allowlist leaves out
for path in /backend/getIP.php/../getIP_util.php /backend/getIP.php/%2e%2e/getIP_util.php \
        /backend/getIP.php/../../results/telemetry.php /backend/empty.php/%2e%2e/%2e%2e/results/stats.php; do
    assert_status 404 "GET ${path}" --path-as-is "${BASE}${path}"
done

echo '== 6. the speed-test endpoints and the generated config'
download=$(curl -s -o /dev/null -w '%{http_code} %{size_download}' --max-time 30 \
    "${BASE}/backend/garbage.php?ckSize=4")
assert_eq '200 4194304' "${download}" 'GET /backend/garbage.php?ckSize=4: status, bytes'
upload=$(post_upload 6-typed -H 'Content-Type: application/octet-stream')
check_upload 6-typed 'POST /backend/empty.php, 20 MiB of application/octet-stream' "${upload}"
# XMLHttpRequest sends speedtest_worker.js's untyped Blob with no Content-Type
upload=$(post_upload 6-untyped -H 'Content-Type:')
check_upload 6-untyped 'POST /backend/empty.php, 20 MiB with no Content-Type, as browsers send it' "${upload}"
if body=$(getip_body "${CTR}"); then
    assert_eq "${GETIP_LOOPBACK}" "${body}" 'GET /backend/getIP.php over loopback'
else
    fail "GET /backend/getIP.php over loopback: ${body}"
fi
# Answered from the bundled mmdb through the phar: there is no egress
isp=$(curl -s -w '\n%{http_code}' --max-time 5 -H 'X-Forwarded-For: 1.1.1.1' "${BASE}/backend/getIP.php?isp")
assert_eq 200 "${isp##*$'\n'}" 'GET /backend/getIP.php?isp for X-Forwarded-For 1.1.1.1'
processed=$(jq -r '.processedString' 2>&1 <<<"${isp%$'\n'*}")
if [[ "${processed}" == *Cloudflare* || "${processed}" == *AS13335* ]]; then
    pass "1.1.1.1 resolves to Cloudflare (${processed})"
else
    fail "1.1.1.1 resolves to Cloudflare: got '${processed}'"
fi
list=$(curl -s --max-time 5 "${BASE}/server-list.json")
if jq -e 'type == "array" and length == 1' >/dev/null 2>&1 <<<"${list}"; then
    pass 'server-list.json is a one-element array'
else
    fail "server-list.json is a one-element array: got '${list}'"
fi
assert_lacks '://' 'server-list.json holds no absolute URL' printf '%s' "${list}"
servers=$(curl -s --max-time 5 "${BASE}/servers.json")
if [[ -n "${list}" && "${servers}" == "${list}" ]]; then
    pass 'servers.json is the same list'
else
    fail "servers.json is the same list: got '${servers}'"
fi
settings=$(curl -s --max-time 5 "${BASE}/settings.json")
telemetry_level=$(jq -r '.telemetry_level' 2>&1 <<<"${settings}")
test_order=$(jq -r '.test_order' 2>&1 <<<"${settings}")
assert_eq off "${telemetry_level}" 'settings.json telemetry_level'
assert_eq ID_U_P "${test_order}" "settings.json keeps upstream's test_order"

echo "== 7. three parallel uploads within --tmpfs /tmp:size=${TMPFS_SIZE}"
for i in 1 2 3; do
    post_upload "7-${i}" -H 'Content-Type: application/octet-stream' >"${WORK}/upload-${i}" &
done
wait
for i in 1 2 3; do
    check_upload "7-${i}" "parallel upload ${i} of 3" "$(<"${WORK}/upload-${i}")"
done
assert_status 200 '/backend/getIP.php after the uploads' "${BASE}/backend/getIP.php"

echo '== 8. hostile TITLE, TAGLINE and GDPR_EMAIL render escaped; a bad TELEMETRY stops the start'
"${ENGINE}" run -d --name "${HOSTILE_CTR}" "${CONFINE[@]}" \
    --network "${NET}" -p "${HOSTILE_PORT}:8080" \
    -e "TITLE=Tést <b>&'\"" -e 'TAGLINE=<script>x</script>' -e 'GDPR_EMAIL=a@b.example' \
    "${IMAGE}" >/dev/null
if ! wait_for_port "${HOSTILE_BASE}/"; then
    fail 'the container with hostile values never served'
    "${ENGINE}" logs "${HOSTILE_CTR}"
fi
page=$(curl -s --max-time 5 "${HOSTILE_BASE}/")
# htmlspecialchars(..., ENT_QUOTES | ENT_HTML5, 'UTF-8')
title='Tést &lt;b&gt;&amp;&apos;&quot;'
assert_contains "<title>${title} - Free and Open Source Speedtest</title>" 'TITLE in <title>, escaped' \
    printf '%s' "${page}"
assert_contains "<h1>${title}</h1>" 'TITLE in <h1>, escaped' printf '%s' "${page}"
assert_contains '<p class="tagline">&lt;script&gt;x&lt;/script&gt;</p>' 'TAGLINE, escaped' \
    printf '%s' "${page}"
assert_lacks '<b>' 'no raw <b>' printf '%s' "${page}"
assert_lacks '<script>x' 'no raw <script>x' printf '%s' "${page}"
assert_contains 'href="mailto:a@b.example"' 'GDPR_EMAIL in the mailto: link' printf '%s' "${page}"
assert_contains '>a@b.example</a>' "GDPR_EMAIL in place of upstream's placeholder text" printf '%s' "${page}"
assert_lacks 'PUT@YOUR_EMAIL.HERE' "no upstream placeholder address" printf '%s' "${page}"
"${ENGINE}" run -d --name "${BADENV_CTR}" "${CONFINE[@]}" --network "${NET}" \
    -e TELEMETRY=maybe "${IMAGE}" >/dev/null
exit_code=$(exit_code_within "${BADENV_CTR}" 30)
assert_eq 1 "${exit_code}" 'TELEMETRY=maybe: the container exits'
assert_contains TELEMETRY 'TELEMETRY=maybe: the log names TELEMETRY' "${ENGINE}" logs "${BADENV_CTR}"

echo
if (( FAILURES == 0 )); then
    echo "acceptance: all checks passed"
    exit 0
fi
echo "acceptance: ${FAILURES} check(s) failed" >&2
exit 1
