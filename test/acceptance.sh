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
# Sections 9 to 12 run one container at a time on this port
TEL_PORT=18192
# Rootless Podman hands the server a published port's connections from the
# server's own address, not the gateway's as Docker does, and a container
# run again draws a new one. So every container on TEL_PORT takes the same
# fixed address, or section 15's restart would face a new client and prove
# nothing about failures kept on /data. The others draw from the lower half.
NET_SUBNET=10.89.66.0/24
NET_RANGE=10.89.66.0/25
TEL_IP=10.89.66.200
TEL_BASE="http://127.0.0.1:${TEL_PORT}"
TEL_CTR=librespeed-acc-telemetry
REFUSED_CTR=librespeed-acc-refused
TEL_VOL=librespeed-acc-telemetry
PERSIST_VOL=librespeed-acc-persist
REFUSED_VOL=librespeed-acc-refused
# Sections 13 to 15 use TEL_CTR and TEL_PORT too
STATS_VOL=librespeed-acc-stats
STATS_URL="${TEL_BASE}/results/stats.php"
STATS_PASSWORD_VALUE=correct-horse
# Section 16: the server, and a second container of the same image in front
# of it as its proxy, on a network whose addresses the server trusts
PROXY_NET=librespeed-acc-proxy-net
PROXY_SUBNET=10.89.77.0/24
PROXIED_CTR=librespeed-acc-proxied
PROXIED_IP=10.89.77.10
PROXIED_VOL=librespeed-acc-proxied
PROXY_CTR=librespeed-acc-proxy
PROXY_IP=10.89.77.20
# Sections 17 to 19 use TEL_CTR and TEL_PORT too, and publish the health
# listener, which a deployment keeps unpublished, only to test it
HEALTH_PORT=18193
HEALTH_BASE="http://127.0.0.1:${HEALTH_PORT}"
HEALTH_VOL=librespeed-acc-health

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

# The table upstream's getPdo() makes, as results_table prints it, before
# any result is recorded
EMPTY_RESULTS_TABLE='{"columns":["id","ispinfo","extra","timestamp","ip","ua","lang","dl","ul","ping","jitter","log"],"rows":0}'

# The shipped health probe, and what HEALTHCHECK runs, as its exec form
HEALTH_PROBE=/usr/local/bin/librespeed-health
HEALTH_CMD='["/usr/local/bin/frankenphp","php-cli","/usr/local/bin/librespeed-health"]'
# Podman reads no HEALTHCHECK from an image published as an OCI index, so
# its users pass the same array as --health-cmd, and under Podman so does
# this suite. No leading "CMD": before 5.8.0, Podman re-splits an array
# that starts with it on white space, and never runs the probe.
HEALTH_ARGS=()
if [[ "${ENGINE}" == podman ]]; then
    HEALTH_ARGS=(--health-cmd "${HEALTH_CMD}")
fi
# Read from the image's HEALTHCHECK in section 17, under Docker
HEALTH_START_PERIOD=0

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
# allows, known by its own answer, or there must be a refusal. A helper such
# as getIP_util.php answers 200 with an empty body instead.
DOTSEG_CLIENT=203.0.113.9
assert_runs_or_refused() {
    local base="${1}" path="${2}" script="${3}" expected="${4}" out status body
    out=$(curl -s --path-as-is --max-time 5 -w '\n%{http_code}' \
        -H "X-Forwarded-For: ${DOTSEG_CLIENT}" "${base}${path}")
    status=${out##*$'\n'}
    body=${out%$'\n'*}
    case "${status}" in
        200) assert_eq "${expected}" "${body}" "GET ${path} runs ${script}" ;;
        400 | 404) pass "GET ${path} is refused (${status})" ;;
        *) fail "GET ${path}: expected ${script}'s answer or a refusal, got ${status}" ;;
    esac
}
assert_getip_or_refused() {
    assert_runs_or_refused "${BASE}" "${1}" getIP.php "{\"processedString\":\"${DOTSEG_CLIENT}\",\"rawIspInfo\":\"\"}"
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

# Start a container under the contract on TEL_PORT, with the engine options
# given, then install the probe. Fails, with the container's log, if it
# stops or never serves.
start_served() {
    local ctr="${1}" deadline=$(( SECONDS + WAIT_FOR_PORT_SECONDS )) status running
    shift
    "${ENGINE}" rm -f "${ctr}" >/dev/null 2>&1
    if ! "${ENGINE}" run -d --name "${ctr}" "${CONFINE[@]}" --network "${NET}" --ip "${TEL_IP}" \
            -p "${TEL_PORT}:8080" "${@}" "${IMAGE}" >/dev/null; then
        fail "${ctr}: ${ENGINE} run ${*} failed"
        return 1
    fi
    while :; do
        status=$(http_status "${TEL_BASE}/")
        [[ "${status}" != 000 ]] && break
        running=$("${ENGINE}" inspect --format '{{.State.Running}}' "${ctr}" 2>&1)
        if [[ "${running}" != true ]] || (( SECONDS >= deadline )); then
            fail "${ctr} never served with ${*}"
            "${ENGINE}" logs "${ctr}" 2>&1 | sed 's/^/       | /' >&2
            return 1
        fi
        sleep 1
    done
    install_probe "${ctr}"
}

# Start a container that must refuse to start, and check that its log
# names the reason
assert_refused() {
    local what="${1}" needle="${2}" code
    shift 2
    "${ENGINE}" rm -f "${REFUSED_CTR}" >/dev/null 2>&1
    "${ENGINE}" run -d --name "${REFUSED_CTR}" "${CONFINE[@]}" --network "${NET}" "${@}" "${IMAGE}" >/dev/null
    code=$(exit_code_within "${REFUSED_CTR}" 30)
    assert_eq 1 "${code}" "${what}: the container exits"
    assert_contains "${needle}" "${what}: the log says '${needle}'" "${ENGINE}" logs "${REFUSED_CTR}"
}

# Run PHP code in a throwaway container as uid 65532, the volume's owner
php_on_volume() {
    local volume="${1}" code="${2}"
    "${ENGINE}" run --rm "${CONFINE[@]}" --network none -v "${volume}:/data" \
        --entrypoint "${PHP_BIN}" "${IMAGE}" php-cli -r "${code}"
}

# Run PHP code in a running container and print what it prints
php_in() {
    "${ENGINE}" exec "${1}" "${PHP_BIN}" php-cli -r "${2}" 2>&1
}

# Run one of the probe's printing subcommands in the telemetry container. On
# failure it prints the probe's FAIL record, which no check will take for an
# answer.
probe_out() {
    "${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" "${@}" 2>&1
}

# What a browser at telemetry level basic posts at the end of a test.
# speedtest_worker.js sends FormData, so multipart, with the fields in this
# order: ispinfo is JSON.stringify({processedString, rawIspInfo}) of
# getIP.php's answer with ISP info, log is empty below level full, and
# extra names the server the UI selected. The values are as the worker
# formats them, to two decimals.
RESULT_DL=8.42
RESULT_UL=1.07
RESULT_PING=23.41
RESULT_JITTER=3.18
BROWSER_UA='Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0'
# The client's address, as X-Forwarded-For carries it to upstream's
# getClientIp(). A long ISP name, with characters outside ASCII.
RESULT_CLIENT=186.251.0.1
# json.php's answer for the result above, less its timestamp: the speeds as
# upstream formats them, and the ISP from the text after the address
RESULT_JSON='{"download":"8.42","upload":"1.07","ping":"23.4","jitter":"3.18","ispinfo":"OnLine Assis Telecomunicações Ltda-EPP, Brazil"}'

# Post that result. POST_META is "<curl exit> <status> <bytes sent>" and
# POST_BODY the answer.
post_result() {
    local getip ispinfo out rc
    getip=$(curl -s --max-time 5 -H "X-Forwarded-For: ${RESULT_CLIENT}" \
        "${TEL_BASE}/backend/getIP.php?isp=true&distance=km&r=0.5")
    ispinfo=$(jq -c '{processedString, rawIspInfo}' 2>/dev/null <<<"${getip}")
    out=$(curl -s --max-time 10 -w '\n%{http_code} %{size_upload}' \
        -A "${BROWSER_UA}" -H 'Accept-Language: pt-BR,pt;q=0.9' -H "X-Forwarded-For: ${RESULT_CLIENT}" \
        --form-string "ispinfo=${ispinfo}" --form-string "dl=${RESULT_DL}" --form-string "ul=${RESULT_UL}" \
        --form-string "ping=${RESULT_PING}" --form-string "jitter=${RESULT_JITTER}" --form-string 'log=' \
        --form-string 'extra={"server":"local"}' "${TEL_BASE}/results/telemetry.php?r=0.42")
    rc=$?
    POST_META="${rc} ${out##*$'\n'}"
    POST_BODY=${out%$'\n'*}
}

# Post the result. RESULT_ID is the id telemetry.php answers with, and
# RESULT_BYTES the size of the body sent.
record_result() {
    local what="${1}"
    RESULT_ID=''
    post_result
    RESULT_BYTES=${POST_META##* }
    if [[ "${POST_META}" == "0 200 "* && "${POST_BODY}" == 'id '* ]]; then
        pass "${what}: POST /results/telemetry.php answers '${POST_BODY}'"
        RESULT_ID=${POST_BODY#id }
    else
        fail "${what}: POST /results/telemetry.php: expected '0 200 <bytes>' and 'id <id>', got '${POST_META}' and '${POST_BODY}'"
    fi
}

# json.php's answer for an id, less the timestamp, which no test can know
result_json() {
    curl -s --max-time 5 "${TEL_BASE}/results/json.php?id=${1}" | jq -c 'del(.timestamp)' 2>&1
}

# A share image is 200 image/png and starts with PNG's signature. index.php
# answers 200 with an empty body for an id it cannot find.
assert_png() {
    local url="${1}" what="${2}" meta magic
    meta=$(curl -s --max-time 10 -o "${WORK}/image" -w '%{http_code} %{content_type}' "${url}")
    magic=$(od -An -tx1 -N8 "${WORK}/image" | tr -d ' \n')
    assert_eq '200 image/png 89504e470d0a1a0a' "${meta} ${magic}" "${what}: status, type, signature"
}

# The variables of the generated telemetry_settings.php, as JSON, as
# upstream's scripts see them once they require it
telemetry_settings() {
    php_in "${1}" 'require "/tmp/librespeed/telemetry_settings.php";
        echo json_encode(compact("db_type", "stats_password", "enable_id_obfuscation",
            "redact_ip_addresses", "Sqlite_db_file")), "\n";'
}

# The salt read again in section 12, which must be a salt, and the one
# first read there
assert_same_salt() {
    local what="${1}" again
    again=$(probe_out read-salt)
    if [[ "${salt_format}" == true && "${again}" == "${salt}" ]]; then
        pass "${what}: the same salt"
    else
        fail "${what}: the same salt: expected '${salt}', got '${again}'"
    fi
}

# Every way a client might write a login to stats.php. PHP takes the last of
# a repeated key, and Caddy's query matcher matches any of them. PHP also
# drops a key's leading spaces and cuts it at a NUL, while Caddy matches a
# key as written and refuses to match at all on a query it cannot parse: a
# stray ; or a bad escape. So several of these say op=login to PHP but not
# to Caddy.
STATS_PATHS=(
    '/results/stats.php'
    '/results/stats.php?op=login'
    '/results/stats.php?op=x&op=login'
    '/results/stats.php?op=login&op=x'
    '/results/stats.php?op=%6cogin'
    '/results/stats.php?%6fp=login'
    '/results/stats.php?OP=login'
    '/results/stats.php?op=LOGIN'
    '/results/stats.php?op%00=login'
    '/results/stats.php?%20op=login'
    '/results/stats.php?+op=login'
    '/results/stats.php?x=;&op=login'
    '/results/stats.php?op=login&x=%zz'
    '/results/stats.php?op[]=login'
    '/RESULTS/STATS.PHP?op=login'
    '/results/./stats.php?op%00=login'
    '/results//stats.php?%20op=login'
    '/results/%73tats.php?op%00=login'
    '/results/json.php/../stats.php?op=login'
    '/results/json.php/%2e%2e/stats.php?op%00=login'
    '/backend/getIP.php/../../results/stats.php?x=;&op=login'
)

# Paths that start at stats.php and resolve to another script, each beside
# that script's own path. Raw, such a path would split at its first .php and
# run stats.php, with whatever was posted to it.
STATS_EXITS=(
    '/results/stats.php/../../backend/getIP.php?op=login /backend/getIP.php?op=login'
    '/results/stats.php/%2e%2e/%2e%2e/backend/getIP.php?op=login /backend/getIP.php?op=login'
    '/results/stats.php/..%2f..%2fbackend/getIP.php?op=login /backend/getIP.php?op=login'
    '/RESULTS/STATS.PHP/../../backend/getIP.php?op=login /backend/getIP.php?op=login'
    '/results/stats.php/.%2e/.%2e/backend/empty.php?op=login /backend/empty.php?op=login'
    '/results/stats.php/../json.php?op=login&id=1 /results/json.php?op=login&id=1'
    '/results/stats.php/%2e%2e/json.php?op=login&id=1 /results/json.php?op=login&id=1'
    '/results/stats.php/../../results/json.php?op=login&id=1 /results/json.php?op=login&id=1'
    '/results/stats.php/../telemetry.php?op=login /results/telemetry.php?op=login'
    '/results/stats.php/%2E%2E/telemetry.php?op=login /results/telemetry.php?op=login'
    '/results/stats.php/../sanitycheck.php?op=login /results/sanitycheck.php?op=login'
)
# Upstream's stats page and form, and login.php's every answer, carry it
STATS_TITLE='<title>LibreSpeed - Stats</title>'

# One request as assert_leaves_stats sends it: the body, with any id
# telemetry.php hands out made N, then the status
stats_exit_request() {
    local base="${1}" method="${2}" path="${3}" password="${4}"
    if [[ "${method}" == POST ]]; then
        curl -s -g --path-as-is --max-time 10 -w '\n%{http_code}' -H "X-Forwarded-For: ${DOTSEG_CLIENT}" \
            --data-urlencode "password=${password}" "${base}${path}"
    else
        curl -s -g --path-as-is --max-time 10 -w '\n%{http_code}' -H "X-Forwarded-For: ${DOTSEG_CLIENT}" \
            "${base}${path}"
    fi | sed 's/^id [0-9]*$/id N/'
}

# A path out of stats.php is answered by the script it resolves to, exactly
# as that script answers when asked directly, or it is refused. Neither
# stats.php nor its login ever answers it.
assert_leaves_stats() {
    local base="${1}" method="${2}" path="${3}" target="${4}" password="${5}" got want status
    got=$(stats_exit_request "${base}" "${method}" "${path}" "${password}")
    want=$(stats_exit_request "${base}" "${method}" "${target}" "${password}")
    status=${got##*$'\n'}
    if [[ "${got}" == *"${STATS_TITLE}"* ]]; then
        fail "${method} ${path}: stats.php or its login answered (${status})"
    elif [[ "${status}" == 400 || "${status}" == 404 ]]; then
        pass "${method} ${path} is refused (${status})"
    else
        assert_eq "${want}" "${got}" "${method} ${path} runs ${target%%\?*}"
    fi
}

# POST a password to a stats URL as upstream's login form does, with the
# cookie jar given. Sets STATUS, LOCATION, RETRY_AFTER, SENT (the bytes of
# the body) and BODY.
stats_login() {
    local url="${1}" password="${2}" jar="${3}" out
    shift 3
    out=$(curl -s -g --path-as-is --max-time 10 -c "${jar}" -b "${jar}" -D "${WORK}/headers" \
        -o "${WORK}/body" -w '%{http_code} %{size_upload}' --data-urlencode "password=${password}" \
        "${@}" "${url}")
    STATUS=${out% *}
    SENT=${out#* }
    LOCATION=$(tr -d '\r' <"${WORK}/headers" | awk 'tolower($1) == "location:" { print $2 }')
    RETRY_AFTER=$(tr -d '\r' <"${WORK}/headers" | awk 'tolower($1) == "retry-after:" { print $2 }')
    BODY=$(<"${WORK}/body")
}

# The stats page as the holder of a cookie jar sees it
stats_page() {
    curl -s --max-time 10 -c "${1}" -b "${1}" "${STATS_URL}"
}

# The PHP session id a cookie jar holds
session_id() {
    awk '$6 == "PHPSESSID" { id = $7 } END { print id }' "${1}" 2>/dev/null
}

# login.php's failures still inside its window, as client=count lines. It
# opens the database read-only, so it never creates one.
failure_buckets() {
    php_in "${1}" 'echo json_encode((new PDO("sqlite:/data/stats-login.db", null, null,
            [Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY]))
        ->query("SELECT client, COUNT(*) FROM failures WHERE at > " . (time() - 900) . " GROUP BY client ORDER BY client")
        ->fetchAll(PDO::FETCH_KEY_PAIR)), "\n";' | jq -r 'to_entries[] | "\(.key)=\(.value)"' 2>&1
}

# A generated file, as the running server reads it
generated_file() {
    php_in "${1}" "readfile('/tmp/librespeed/${2}');"
}

# The client-address options of every server, as the running server adapts
# its own Caddyfile
client_ip_options() {
    "${ENGINE}" exec "${1}" "${PHP_BIN}" adapt --config /etc/frankenphp/Caddyfile 2>/dev/null \
        | jq -c '[.apps.http.servers[] | {trusted_proxies, trusted_proxies_strict, client_ip_headers}] | unique' 2>&1
}

# One request from the proxy container to the server behind it. Sets
# PROXY_STATUS. On failure it is the probe's FAIL record, which no check
# takes for a status.
proxy_request() {
    local method="${1}" path="${2}" headers="${3}" body="${4}" out
    out=$("${ENGINE}" exec "${PROXY_CTR}" "${PHP_BIN}" php-cli "${PROBE}" http-request "${method}" \
        "http://${PROXIED_IP}:8080${path}" "${headers}" "${body}" 2>&1)
    PROXY_STATUS=${out%%$'\n'*}
}

# A login from inside the server's container, so from 127.0.0.1: a client
# apart from this host. With http-request-locked it is sent while the probe
# holds login.php's lock. Sets LOOPBACK_STATUS and LOOPBACK_BODY.
loopback_login() {
    local command="${1}" password="${2}" out
    out=$("${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" "${command}" POST \
        'http://127.0.0.1:8080/results/stats.php?op=login' '' "password=${password}" 2>&1)
    LOOPBACK_STATUS=${out%%$'\n'*}
    LOOPBACK_BODY=${out#*$'\n'}
}

# Who holds login.php's lock, from /proc/locks: "<kind> <access> <pid>", or
# nothing
login_lock_holders() {
    "${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" lock-holders /tmp/librespeed/stats-login.lock 2>&1
}

# The results table in a running container's database, as JSON: its columns
# and how many rows it holds. It opens the database read-only, so it never
# creates one.
results_table() {
    php_in "${1}" 'function db(): PDO { return new PDO("sqlite:/data/telemetry.db", null, null,
            [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY]); }
        echo json_encode(["columns" => array_column(db()->query("PRAGMA table_info(speedtest_users)")->fetchAll(), "name"),
            "rows" => (int) db()->query("SELECT COUNT(*) FROM speedtest_users")->fetchColumn()]), "\n";'
}

# GET /healthz on the published health listener. Sets HEALTHZ_STATUS, and
# HEALTHZ_BODY without its final newline.
healthz() {
    local out
    out=$(curl -s --max-time 10 -w '\n%{http_code}' "${HEALTH_BASE}/healthz")
    HEALTHZ_STATUS=${out##*$'\n'}
    HEALTHZ_BODY=${out%$'\n'*}
    HEALTHZ_BODY=${HEALTHZ_BODY%$'\n'}
}

# The shipped probe, run in a container as HEALTHCHECK runs it. Sets
# HEALTH_RC, and HEALTH_OUT to everything it printed.
run_health_probe() {
    HEALTH_OUT=$("${ENGINE}" exec "${1}" "${PHP_BIN}" php-cli "${HEALTH_PROBE}" 2>&1)
    HEALTH_RC=$?
}

# The engine's own verdict, which it reaches by running the health check
# itself: Docker runs the image's HEALTHCHECK, Podman the --health-cmd it
# was given. A shell-form command needs /bin/sh, which the image lacks, so
# only the exec form can pass. Docker must say healthy within the start
# period of the time given; Podman runs one check when asked.
assert_engine_healthy() {
    local ctr="${1}" started="${2}" what="${3}" status out rc deadline
    if [[ "${ENGINE}" == podman ]]; then
        out=$("${ENGINE}" healthcheck run "${ctr}" 2>&1)
        rc=$?
        assert_eq 0 "${rc}" "${what}: podman healthcheck run"
        (( rc == 0 )) || printf '       | %s\n' "${out}" >&2
        return
    fi
    deadline=$(( started + HEALTH_START_PERIOD ))
    while :; do
        status=$("${ENGINE}" inspect --format \
            '{{if .State.Health}}{{.State.Health.Status}}{{else}}no health check{{end}}' "${ctr}" 2>&1)
        if [[ "${status}" == healthy || "${status}" == unhealthy ]] || (( SECONDS >= deadline )); then
            break
        fi
        sleep 1
    done
    assert_eq healthy "${status}" "${what}: ${ENGINE} reports it healthy within the ${HEALTH_START_PERIOD} s start period"
    if [[ "${status}" != healthy ]]; then
        "${ENGINE}" inspect --format '{{if .State.Health}}{{json .State.Health.Log}}{{end}}' "${ctr}" 2>&1 \
            | sed 's/^/       | /' >&2
    fi
}

# /healthz is 503 and names the failed check on a line of its own, and the
# probe exits 1, naming the /healthz half and that check. The getIP.php
# half, still sound, goes unnamed.
assert_health_fails() {
    local what="${1}" check="${2}"
    healthz
    assert_eq 503 "${HEALTHZ_STATUS}" "${what}: GET :8081/healthz"
    if [[ $'\n'"${HEALTHZ_BODY}" == *$'\n'"${check}:"* ]]; then
        pass "${what}: /healthz names ${check} (${HEALTHZ_BODY//$'\n'/ | })"
    else
        fail "${what}: /healthz names ${check}: got '${HEALTHZ_BODY//$'\n'/ | }'"
    fi
    run_health_probe "${TEL_CTR}"
    assert_eq 1 "${HEALTH_RC}" "${what}: ${HEALTH_PROBE} exits 1"
    if [[ "${HEALTH_OUT}" == *'http://127.0.0.1:8081/healthz'*"${check}:"* && "${HEALTH_OUT}" != *getIP.php* ]]; then
        pass "${what}: the probe names the /healthz half and ${check}, and only that half"
    else
        fail "${what}: the probe names the /healthz half and ${check}, and only that half: got '${HEALTH_OUT}'"
    fi
}

cleanup() {
    "${ENGINE}" rm -f "${CTR}" "${INTERNAL_CTR}" "${HOSTILE_CTR}" "${BADENV_CTR}" "${TEL_CTR}" \
        "${REFUSED_CTR}" "${PROXIED_CTR}" "${PROXY_CTR}" >/dev/null 2>&1 || true
    "${ENGINE}" network rm -f "${NET}" "${INTERNAL_NET}" "${PROXY_NET}" >/dev/null 2>&1 || true
    "${ENGINE}" volume rm -f "${TEL_VOL}" "${PERSIST_VOL}" "${REFUSED_VOL}" "${STATS_VOL}" \
        "${PROXIED_VOL}" "${HEALTH_VOL}" >/dev/null 2>&1 || true
}
trap 'cleanup; rm -rf "${WORK}"' EXIT
cleanup

"${ENGINE}" network create --subnet "${NET_SUBNET}" --ip-range "${NET_RANGE}" "${NET}" >/dev/null

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

echo '== 4. PHP refuses URL streams, logs real errors to stderr only and keeps bodies off /tmp'
if ini=$("${ENGINE}" exec "${CTR}" "${PHP_BIN}" php-cli "${PROBE}" php-ini 2>&1) \
        && jq -e 'type == "object"' >/dev/null 2>&1 <<<"${ini}"; then
    allow_url_fopen=$(jq -r '.allow_url_fopen' <<<"${ini}")
    display_errors=$(jq -r '.display_errors' <<<"${ini}")
    log_errors=$(jq -r '.log_errors' <<<"${ini}")
    error_log=$(jq -r '.error_log' <<<"${ini}")
    error_reporting=$(jq -r '.error_reporting | join(" ")' <<<"${ini}")
    post_max_size=$(jq -r '.post_max_size_bytes' <<<"${ini}")
    assert_eq false "${allow_url_fopen}" 'allow_url_fopen'
    assert_eq false "${display_errors}" 'display_errors'
    assert_eq true "${log_errors}" 'log_errors'
    # Unset, so PHP hands each line to FrankenPHP's logger: uid 65532 cannot
    # open /dev/stderr, and a file would keep errors out of the container log
    assert_eq '' "${error_log}" 'error_log is unset'
    # Real errors and uncaught exceptions, which are E_ERROR, and nothing a
    # client could raise by the thousand: no warning, notice or deprecation
    assert_eq 'E_ERROR E_PARSE E_CORE_ERROR E_COMPILE_ERROR E_USER_ERROR E_RECOVERABLE_ERROR' \
        "${error_reporting}" 'error_reporting'
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
# FrankenPHP logs PHP's errors as the php handler. The server's own config,
# as the server adapts it, sends that logger to stderr and keeps it out of
# the default log on stdout.
if logging=$("${ENGINE}" exec "${CTR}" "${PHP_BIN}" adapt --config /etc/frankenphp/Caddyfile 2>/dev/null \
        | jq -c '.logging.logs
            | {php: [to_entries[] | select((.value.include // []) | index("http.handlers.php")) | .value.writer.output],
               default: .default.writer.output,
               default_excludes_php: ((.default.exclude // []) | index("http.handlers.php") != null)}' 2>&1); then
    assert_eq '{"php":["stderr"],"default":"stdout","default_excludes_php":true}' "${logging}" \
        "PHP's error log goes to stderr, and the rest of the server's log to stdout"
else
    fail "PHP's error log goes to stderr, not stdout: could not adapt the server's Caddyfile: ${logging}"
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

echo '== 9. telemetry off, the default: no results routes, no volume, its switches ignored'
# The container from section 1, which has no volume and sets nothing
for path in /results/telemetry.php /results/json.php /results/index.php /results/ \
        '/results/json.php?id=1' '/results/?id=1&style=modern'; do
    assert_status 404 "GET ${path}" "${BASE}${path}"
done
assert_status 404 'POST /results/telemetry.php' -X POST --form-string "dl=${RESULT_DL}" \
    "${BASE}/results/telemetry.php"
# Nor do they answer from an allowed route by way of a dot segment
for path in /backend/getIP.php/../../results/json.php /backend/getIP.php/%2e%2e/%2e%2e/results/index.php \
        /backend/empty.php/../../results/telemetry.php /frontend/../results/json.php \
        /images/..%2fresults/telemetry.php /backend/garbage.php/.%2e/.%2e/results/; do
    assert_status 404 "GET ${path}" --path-as-is "${BASE}${path}"
done
state=$(php_in "${CTR}" 'readfile("/tmp/librespeed/state.json");' | jq -c . 2>&1)
assert_eq '{"telemetry":false}' "${state}" 'state.json'
generated=$(php_in "${CTR}" 'echo file_exists("/tmp/librespeed/telemetry_settings.php") ? "present" : "absent";')
assert_eq absent "${generated}" 'no telemetry_settings.php'
# Telemetry's own switches, set while it is off, are named and ignored: the
# image's read-only /data is not touched for a salt
start_served "${TEL_CTR}" -e REDACT_IP_ADDRESSES=true -e ENABLE_ID_OBFUSCATION=true
assert_contains 'TELEMETRY is off, so these are ignored: REDACT_IP_ADDRESSES, ENABLE_ID_OBFUSCATION' \
    'REDACT_IP_ADDRESSES and ENABLE_ID_OBFUSCATION without TELEMETRY: the log says they are ignored' \
    "${ENGINE}" logs "${TEL_CTR}"
assert_status 404 'GET /results/json.php with only the switches set' "${TEL_BASE}/results/json.php"
level=$(curl -s --max-time 5 "${TEL_BASE}/settings.json" | jq -r '.telemetry_level' 2>&1)
assert_eq off "${level}" 'settings.json telemetry_level with only the switches set'
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1
# Ignored is not unchecked: a value that is neither true nor false still
# stops the start
assert_refused 'ENABLE_ID_OBFUSCATION=1 without TELEMETRY' ENABLE_ID_OBFUSCATION -e ENABLE_ID_OBFUSCATION=1

echo '== 10. TELEMETRY=true refuses a /data that uid 65532 cannot write, and a database SQLite cannot open'
assert_refused "TELEMETRY=true with no volume, so on the image's read-only /data" \
    'needs a writable /data volume' -e TELEMETRY=true
# Root's, as a host directory would be. A tmpfs behaves the same under
# rootless Podman, where a host directory's owner is mapped.
assert_refused 'TELEMETRY=true on a /data owned by root, mode 0755' \
    'needs a writable /data volume' --tmpfs /data:mode=0755 -e TELEMETRY=true
# A database SQLite would open read-only, and an obfuscation salt PHP could
# not read, which would change every id it hands out
"${ENGINE}" volume create "${REFUSED_VOL}" >/dev/null
php_on_volume "${REFUSED_VOL}" 'touch("/data/telemetry.db"); chmod("/data/telemetry.db", 0444);'
assert_refused 'TELEMETRY=true with a /data/telemetry.db uid 65532 cannot write' \
    '/data/telemetry.db is there, but uid 65532 cannot read and write it' -v "${REFUSED_VOL}:/data" -e TELEMETRY=true
php_on_volume "${REFUSED_VOL}" 'chmod("/data/telemetry.db", 0644);
    file_put_contents("/data/idObfuscation_salt.php", "<?php\n");
    chmod("/data/idObfuscation_salt.php", 0);'
assert_refused 'ENABLE_ID_OBFUSCATION=true with a salt uid 65532 cannot read' \
    '/data/idObfuscation_salt.php is there, but uid 65532 cannot read it' -v "${REFUSED_VOL}:/data" \
    -e TELEMETRY=true -e ENABLE_ID_OBFUSCATION=true
# A database file SQLite does not recognise, as a disk fault or a
# half-restored backup leaves it. Served, every result would be answered
# with an empty 200 and recorded nowhere.
php_on_volume "${REFUSED_VOL}" 'file_put_contents("/data/telemetry.db", str_repeat("x", 4096));'
assert_refused 'TELEMETRY=true with a /data/telemetry.db that is not a database' \
    'cannot open /data/telemetry.db: SQLSTATE[HY000]: General error: 26 file is not a database' \
    -v "${REFUSED_VOL}:/data" -e TELEMETRY=true
"${ENGINE}" rm -f "${REFUSED_CTR}" >/dev/null 2>&1

echo '== 11. telemetry on, recording to SQLite on a named volume'
"${ENGINE}" volume create "${TEL_VOL}" >/dev/null
start_served "${TEL_CTR}" -v "${TEL_VOL}:/data" -e TELEMETRY=true
# Made at start by upstream's own code, before any result: the health check
# needs it, and upstream would make it only with the first result
table=$(results_table "${TEL_CTR}")
assert_eq "${EMPTY_RESULTS_TABLE}" "${table}" "/data/telemetry.db at start: upstream's table, with no rows"
assert_contains 'created /data/telemetry.db' 'the log says the bootstrap created /data/telemetry.db' \
    "${ENGINE}" logs "${TEL_CTR}"
settings=$(curl -s --max-time 5 "${TEL_BASE}/settings.json")
level=$(jq -r '.telemetry_level' 2>&1 <<<"${settings}")
assert_eq basic "${level}" 'settings.json telemetry_level'
# The worker posts telemetry_extra in every record, wrapped with the
# server's name, so the image leaves it to the worker
extra=$(jq -r 'has("telemetry_extra")' 2>&1 <<<"${settings}")
assert_eq false "${extra}" 'settings.json sets no telemetry_extra'
state=$(php_in "${TEL_CTR}" 'readfile("/tmp/librespeed/state.json");' | jq -c . 2>&1)
assert_eq '{"telemetry":true}' "${state}" 'state.json'
generated=$(telemetry_settings "${TEL_CTR}")
shape=$(jq -c '.stats_password |= test("\\A[0-9a-f]{64}\\z")' 2>&1 <<<"${generated}")
assert_eq '{"db_type":"sqlite","stats_password":true,"enable_id_obfuscation":false,"redact_ip_addresses":false,"Sqlite_db_file":"/data/telemetry.db"}' \
    "${shape}" 'telemetry_settings.php, with stats_password 64 hex digits'
password=$(jq -r '.stats_password // empty' 2>/dev/null <<<"${generated}")
if [[ -n "${password}" ]]; then
    assert_lacks "${password}" 'the log never shows stats_password' "${ENGINE}" logs "${TEL_CTR}"
else
    fail 'the log never shows stats_password: there is no stats_password to look for'
fi

record_result 'a realistic result'
id=${RESULT_ID}
if ini=$("${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" php-ini 2>&1) \
        && limit=$(jq -er '.post_max_size_bytes' 2>/dev/null <<<"${ini}") \
        && [[ "${RESULT_BYTES}" =~ ^[0-9]+$ ]] && (( RESULT_BYTES > 0 && RESULT_BYTES < limit )); then
    pass "the record fits post_max_size (${RESULT_BYTES} of ${limit} bytes)"
else
    fail "the record fits post_max_size: sent '${RESULT_BYTES}' bytes, the limit is '${limit:-unknown}'"
fi
answer=$(result_json "${id}")
assert_eq "${RESULT_JSON}" "${answer}" "GET /results/json.php?id=${id}"
assert_png "${TEL_BASE}/results/index.php?id=${id}" "GET /results/index.php?id=${id}"
# The modern UI's share link
assert_png "${TEL_BASE}/results/?id=${id}&style=modern" "GET /results/?id=${id}&style=modern"
db=$(php_in "${TEL_CTR}" 'echo is_file("/data/telemetry.db") ? "present" : "absent";')
assert_eq present "${db}" '/data/telemetry.db'
row=$(probe_out read-row "${id}")
stored=$(jq -c --arg ip "${RESULT_CLIENT}" '{ip, dl, extra, log, ip_in_ispinfo: (.ispinfo | contains($ip))}' \
    2>&1 <<<"${row}")
assert_eq "{\"ip\":\"${RESULT_CLIENT}\",\"dl\":\"${RESULT_DL}\",\"extra\":\"{\\\"server\\\":\\\"local\\\"}\",\"log\":\"\",\"ip_in_ispinfo\":true}" \
    "${stored}" "row ${id} as stored, unredacted"

# No other script in results/ answers, directly or by a dot segment from an
# allowed route: stats.php has a route only behind its own login, and
# sanitycheck.php and telemetry_settings.php none at all
for path in /results/stats.php /results/sanitycheck.php /results/telemetry_settings.php \
        /results/telemetry_db.php /results/idObfuscation.php /results/idObfuscation_salt.php \
        /results/OpenSans-Light.ttf /results/index.html /results; do
    assert_status 404 "GET ${path}" "${TEL_BASE}${path}"
done
for path in /results/json.php/../stats.php /results/json.php/%2e%2e/stats.php \
        /results/telemetry.php/..%2fsanitycheck.php /results/index.php/../telemetry_settings.php \
        /results/index.php/.%2e/telemetry_db.php /results/json.php/../../results/stats.php \
        /results/telemetry.php/../idObfuscation_salt.php; do
    assert_status 404 "GET ${path}" --path-as-is "${TEL_BASE}${path}"
done
# and a path that resolves onto an allowed route runs that route's script
answer=$(curl -s --max-time 5 "${TEL_BASE}/results/json.php?id=${id}")
for path in /results/stats.php/../json.php /results/stats.php/%2e%2e/json.php \
        /results/sanitycheck.php/../json.php /results/telemetry_settings.php/.%2e/json.php \
        /backend/getIP_util.php/../../results/json.php; do
    assert_runs_or_refused "${TEL_BASE}" "${path}?id=${id}" json.php "${answer}"
done

# Restarted on the same volume with the client's address redacted.
# json.php never shows the address, so only the row can show it is gone.
start_served "${TEL_CTR}" -v "${TEL_VOL}:/data" -e TELEMETRY=true -e REDACT_IP_ADDRESSES=true
generated=$(telemetry_settings "${TEL_CTR}")
redact=$(jq -c '{redact_ip_addresses}' 2>&1 <<<"${generated}")
assert_eq '{"redact_ip_addresses":true}' "${redact}" 'telemetry_settings.php with REDACT_IP_ADDRESSES=true'
record_result 'a result with REDACT_IP_ADDRESSES=true'
redacted=${RESULT_ID}
assert_lacks "${RESULT_CLIENT}" "GET /results/json.php?id=${redacted} does not show the client's address" \
    curl -s --max-time 5 "${TEL_BASE}/results/json.php?id=${redacted}"
row=$(probe_out read-row "${redacted}")
stored=$(jq -c --arg ip "${RESULT_CLIENT}" '{ip, ip_in_ispinfo: (.ispinfo | contains($ip))}' 2>&1 <<<"${row}")
assert_eq '{"ip":"0.0.0.0","ip_in_ispinfo":false}' "${stored}" "row ${redacted} as stored, redacted"
answer=$(result_json "${id}")
assert_eq "${RESULT_JSON}" "${answer}" "GET /results/json.php?id=${id} after the restart"

# What the health check sees. A damaged database records nothing; once it
# is gone, upstream makes a new one with the next result.
probe "${TEL_CTR}" damage-db
post_result
assert_eq '0 200' "${POST_META% *}" 'POST /results/telemetry.php to a damaged database: curl exit, status'
assert_eq '' "${POST_BODY}" 'POST /results/telemetry.php to a damaged database records nothing'
probe "${TEL_CTR}" remove-db
record_result 'a result after remove-db'
assert_eq 1 "${RESULT_ID}" 'the first result in a new database'
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo '== 12. the salt and the results survive restarts, and telemetry going off and on'
"${ENGINE}" volume create "${PERSIST_VOL}" >/dev/null
start_served "${TEL_CTR}" -v "${PERSIST_VOL}:/data" -e TELEMETRY=true -e ENABLE_ID_OBFUSCATION=true
salt=$(probe_out read-salt)
salt_format=$(jq '.contents | test("\\A<\\?php\n\n\\$OBFUSCATION_SALT = 0x[0-9a-f]{8};\n\\z")' 2>&1 <<<"${salt}")
assert_eq true "${salt_format}" "the salt is in upstream's format"
mode=$(jq -r '.mode' 2>&1 <<<"${salt}")
assert_eq 0600 "${mode}" 'the salt is private to uid 65532'
generated=$(telemetry_settings "${TEL_CTR}")
first_password=$(jq -r '.stats_password // empty' 2>/dev/null <<<"${generated}")
record_result 'a result under ENABLE_ID_OBFUSCATION=true'
obfuscated=${RESULT_ID}
# Upstream's obfuscated ids are seven base-36 digits. Unobfuscated, the
# first row in a new database is plainly 1.
if [[ "${obfuscated}" =~ ^[0-9a-z]{7}$ && "${obfuscated}" != 1 ]]; then
    pass "the id is obfuscated (${obfuscated})"
else
    fail "the id is obfuscated: expected seven base-36 digits, got '${obfuscated}'"
fi
answer=$(result_json "${obfuscated}")
assert_eq "${RESULT_JSON}" "${answer}" "GET /results/json.php?id=${obfuscated}"

start_served "${TEL_CTR}" -v "${PERSIST_VOL}:/data" -e TELEMETRY=true -e ENABLE_ID_OBFUSCATION=true
assert_same_salt 'after a restart'
answer=$(result_json "${obfuscated}")
assert_eq "${RESULT_JSON}" "${answer}" "after a restart: GET /results/json.php?id=${obfuscated}"
generated=$(telemetry_settings "${TEL_CTR}")
second_password=$(jq -r '.stats_password // empty' 2>/dev/null <<<"${generated}")
if [[ -n "${first_password}" && -n "${second_password}" && "${first_password}" != "${second_password}" ]]; then
    pass 'after a restart: a new stats_password'
else
    fail "after a restart: a new stats_password: got '${first_password}', then '${second_password}'"
fi

start_served "${TEL_CTR}" -v "${PERSIST_VOL}:/data" -e ENABLE_ID_OBFUSCATION=true
assert_status 404 'with telemetry off: GET /results/json.php' "${TEL_BASE}/results/json.php?id=${obfuscated}"
assert_same_salt 'with telemetry off'

start_served "${TEL_CTR}" -v "${PERSIST_VOL}:/data" -e TELEMETRY=true -e ENABLE_ID_OBFUSCATION=true
assert_same_salt 'with telemetry on again'
answer=$(result_json "${obfuscated}")
assert_eq "${RESULT_JSON}" "${answer}" "with telemetry on again: GET /results/json.php?id=${obfuscated}"
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo '== 13. no STATS_PASSWORD: neither stats.php nor the login answers, however it is asked for'
"${ENGINE}" volume create "${STATS_VOL}" >/dev/null
start_served "${TEL_CTR}" -v "${STATS_VOL}:/data" -e TELEMETRY=true
for path in "${STATS_PATHS[@]}" /results/login.php /login.php; do
    assert_status 404 "GET ${path}" -g --path-as-is "${TEL_BASE}${path}"
    assert_status 404 "POST ${path}" -g --path-as-is --data-urlencode "password=${STATS_PASSWORD_VALUE}" \
        "${TEL_BASE}${path}"
done
# Nor does a path that starts at stats.php run it on its way elsewhere
for pair in "${STATS_EXITS[@]}"; do
    read -r path target <<<"${pair}"
    for method in GET POST; do
        assert_leaves_stats "${TEL_BASE}" "${method}" "${path}" "${target}" "${STATS_PASSWORD_VALUE}"
    done
done
hash=$(php_in "${TEL_CTR}" 'echo file_exists("/data/stats-password.hash") ? "present" : "absent";')
assert_eq absent "${hash}" 'no /data/stats-password.hash'
# Set while telemetry is off, it is named and ignored
start_served "${TEL_CTR}" -e "STATS_PASSWORD=${STATS_PASSWORD_VALUE}"
assert_contains 'TELEMETRY is off, so these are ignored: STATS_PASSWORD' \
    'STATS_PASSWORD without TELEMETRY: the log says it is ignored' "${ENGINE}" logs "${TEL_CTR}"
assert_lacks "${STATS_PASSWORD_VALUE}" 'STATS_PASSWORD without TELEMETRY: the log never shows it' \
    "${ENGINE}" logs "${TEL_CTR}"
assert_status 404 'GET /results/stats.php with only STATS_PASSWORD set' "${STATS_URL}"
assert_status 404 'POST /results/stats.php?op=login with only STATS_PASSWORD set' \
    --data-urlencode "password=${STATS_PASSWORD_VALUE}" "${STATS_URL}?op=login"
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1
# bcrypt reads the first 72 bytes and ignores the rest
printf -v long '%073d' 0
assert_refused 'a STATS_PASSWORD of 73 bytes' 'STATS_PASSWORD is 73 bytes long' -e "STATS_PASSWORD=${long}"

echo "== 14. STATS_PASSWORD set: upstream's own form posts into the image's login"
start_served "${TEL_CTR}" -v "${STATS_VOL}:/data" -e TELEMETRY=true -e "STATS_PASSWORD=${STATS_PASSWORD_VALUE}"
hash=$(php_in "${TEL_CTR}" 'const HASH = "/data/stats-password.hash";
    echo json_encode(["mode" => sprintf("%04o", (int) @fileperms(HASH) & 0o7777),
        "algo" => password_get_info((string) @file_get_contents(HASH))["algoName"],
        "verifies" => password_verify("correct-horse", (string) @file_get_contents(HASH)),
        "plain" => str_contains((string) @file_get_contents(HASH), "correct-horse")]), "\n";')
assert_eq '{"mode":"0600","algo":"bcrypt","verifies":true,"plain":false}' "${hash}" '/data/stats-password.hash'
assert_lacks "${STATS_PASSWORD_VALUE}" 'the log never shows STATS_PASSWORD' "${ENGINE}" logs "${TEL_CTR}"
# The server starts without it, so no PHP the server runs can read it back.
# TELEMETRY shows the environment was read at all.
environ=$(php_in "${TEL_CTR}" 'const ENVIRON = "/proc/1/environ";
    echo json_encode(["read" => @file_get_contents(ENVIRON) !== false,
        "TELEMETRY" => str_contains((string) @file_get_contents(ENVIRON), "TELEMETRY=true"),
        "STATS_PASSWORD" => str_contains((string) @file_get_contents(ENVIRON), "STATS_PASSWORD=")]), "\n";')
assert_eq '{"read":true,"TELEMETRY":true,"STATS_PASSWORD":false}' "${environ}" "the server's environment"

jar="${WORK}/jar-14"
rm -f "${jar}"
assert_status 200 'GET /results/stats.php' "${STATS_URL}"
page=$(stats_page "${jar}")
assert_contains '<form action="stats.php?op=login" method="POST">' "GET /results/stats.php is upstream's login form" \
    printf '%s' "${page}"
before=$(session_id "${jar}")
stats_login "${STATS_URL}?op=login" wrong "${jar}"
assert_eq 403 "${STATUS}" 'POST /results/stats.php?op=login with a wrong password'
assert_contains '<form action="stats.php?op=login" method="POST">' 'a wrong password: the form again' \
    printf '%s' "${BODY}"
buckets=$(failure_buckets "${TEL_CTR}")
if [[ "${buckets}" =~ ^[0-9a-f.:/]+=1$ ]]; then
    pass "the wrong password is recorded (${buckets})"
else
    fail "the wrong password is recorded: expected '<address>=1', got '${buckets}'"
fi
limit=$(probe_out php-ini | jq -er '.post_max_size_bytes' 2>/dev/null)
if [[ "${SENT}" =~ ^[0-9]+$ && "${limit}" =~ ^[0-9]+$ ]] && (( SENT > 0 && SENT < limit )); then
    pass "the login fits post_max_size (${SENT} of ${limit} bytes)"
else
    fail "the login fits post_max_size: sent '${SENT}' bytes, the limit is '${limit}'"
fi
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq '303 /results/stats.php' "${STATUS} ${LOCATION}" 'POST /results/stats.php?op=login with the right password'
after=$(session_id "${jar}")
if [[ -n "${before}" && -n "${after}" && "${before}" != "${after}" ]]; then
    pass "the login hands out a new session id (${before} became ${after})"
else
    fail "the login hands out a new session id: got '${before}', then '${after}'"
fi
assert_contains 'value="Logout"' 'following it with the cookie, GET /results/stats.php is logged in' \
    stats_page "${jar}"
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '' "${buckets}" "the login clears that client's failures"
# A session id an attacker planted before the login is not the one logged in
fixation=fixation0123456789abcdefghijklmn
printf '127.0.0.1\tFALSE\t/\tFALSE\t0\tPHPSESSID\t%s\n' "${fixation}" >"${WORK}/jar-fixed"
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${WORK}/jar-fixed"
assert_eq 303 "${STATUS}" 'the right password, with a session id chosen beforehand'
assert_lacks 'value="Logout"' 'the session id chosen beforehand is not logged in' \
    curl -s --max-time 10 -H "Cookie: PHPSESSID=${fixation}" "${STATS_URL}"

# Caddy keeps the login's handle blocks ahead of stats.php's own only by
# their order in site.caddy. A GET with op=login reaches login.php, which
# sends it to the page; upstream would answer it with its form.
for path in '/results/stats.php?op=login' '/results/stats.php?op=x&op=login' '/results/stats.php?op=login&op=x'; do
    result=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --max-time 10 "${TEL_BASE}${path}")
    assert_eq "303 ${STATS_URL}" "${result}" "GET ${path} reaches login.php"
done
# Upstream's own check compares a login with stats_password, random and new
# on every start. Even sent that very value, by any way of writing a login,
# it decides nothing: every POST is login.php's, whose 403 and 429 upstream
# never sends, and nobody is logged in.
secret=$(telemetry_settings "${TEL_CTR}" | jq -r '.stats_password // empty' 2>/dev/null)
if [[ "${secret}" =~ ^[0-9a-f]{64}$ ]]; then
    i=0
    for path in "${STATS_PATHS[@]}"; do
        i=$(( i + 1 ))
        stats_login "${TEL_BASE}${path}" "${secret}" "${WORK}/jar-secret-${i}"
        case "${STATUS}" in
            403 | 429) pass "POST ${path} with upstream's stats_password reaches login.php (${STATUS})" ;;
            *) fail "POST ${path} with upstream's stats_password: expected login.php's 403 or 429, got ${STATUS}" ;;
        esac
        assert_lacks 'value="Logout"' "POST ${path} with upstream's stats_password logs nobody in" \
            stats_page "${WORK}/jar-secret-${i}"
    done
else
    fail "upstream's stats_password: expected 64 hex digits, got '${secret}'"
fi
# and a path that starts at stats.php and leaves it never runs it, even
# posting that same value
for pair in "${STATS_EXITS[@]}"; do
    read -r path target <<<"${pair}"
    for method in GET POST; do
        assert_leaves_stats "${TEL_BASE}" "${method}" "${path}" "${target}" "${secret}"
    done
done
probe "${TEL_CTR}" age-failures 901

# Written on every start: a new password replaces the old at once. bcrypt
# alone would also take the new one with anything after its 72nd byte.
printf -v long '%072d' 0
start_served "${TEL_CTR}" -v "${STATS_VOL}:/data" -e TELEMETRY=true -e "STATS_PASSWORD=${long}"
jar="${WORK}/jar-14-restart"
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq 403 "${STATUS}" 'after a restart with a new STATS_PASSWORD of 72 bytes: the old one'
stats_login "${STATS_URL}?op=login" "${long}x" "${jar}"
assert_eq 403 "${STATUS}" 'after a restart with a new STATS_PASSWORD of 72 bytes: the new one and a byte more'
stats_login "${STATS_URL}?op=login" "${long}" "${jar}"
assert_eq 303 "${STATUS}" 'after a restart with a new STATS_PASSWORD of 72 bytes: the new one'

echo '== 15. five failures lock a client out for 900 s, no header makes a new client, one check at a time'
start_served "${TEL_CTR}" -v "${STATS_VOL}:/data" -e TELEMETRY=true -e "STATS_PASSWORD=${STATS_PASSWORD_VALUE}"
assert_lacks 'trusted_proxies' 'TRUSTED_PROXIES unset: global.caddy trusts no proxy' \
    generated_file "${TEL_CTR}" caddy/global.caddy
options=$(client_ip_options "${TEL_CTR}")
assert_eq '[{"trusted_proxies":null,"trusted_proxies_strict":null,"client_ip_headers":null}]' "${options}" \
    'TRUSTED_PROXIES unset: the server trusts no proxy'
# Every header a client might hope the key comes from: X-Forwarded-For, the
# server variable's own name, and the header PHP would map onto that name
jar="${WORK}/jar-15"
for i in 1 2 3 4 5; do
    stats_login "${STATS_URL}?op=login" "wrong-${i}" "${jar}" -H "X-Forwarded-For: 203.0.113.${i}" \
        -H "LIBRESPEED_CLIENT_IP: 198.51.100.${i}" -H "Librespeed-Client-Ip: 198.51.100.${i}"
    assert_eq 403 "${STATUS}" "wrong password ${i} of 5, as 203.0.113.${i} and 198.51.100.${i}"
done
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}" -H 'X-Forwarded-For: 203.0.113.6' \
    -H 'LIBRESPEED_CLIENT_IP: 198.51.100.6' -H 'Librespeed-Client-Ip: 198.51.100.6' \
    -H 'X-Real-IP: 198.51.100.7' -H 'Client-Ip: 198.51.100.8'
assert_eq 429 "${STATUS}" 'the right password after five failures, under new addresses in every header'
if [[ "${RETRY_AFTER}" =~ ^[0-9]+$ ]] && (( RETRY_AFTER >= 1 && RETRY_AFTER <= 900 )); then
    pass "429 says when to try again (Retry-After: ${RETRY_AFTER})"
else
    fail "429 says when to try again: expected Retry-After of 1 to 900 seconds, got '${RETRY_AFTER}'"
fi
buckets=$(failure_buckets "${TEL_CTR}")
if [[ "${buckets}" =~ ^([0-9a-f.:/]+)=5$ && "${BASH_REMATCH[1]}" != 203.0.113.* \
        && "${BASH_REMATCH[1]}" != 198.51.100.* ]]; then
    pass "one client, keyed on the connection's own address, with five failures (${buckets})"
else
    fail "one client, keyed on the connection's own address, with five failures: got '${buckets}'"
fi
stats_login "${STATS_URL}?op=login" wrong-6 "${jar}"
assert_eq 429 "${STATUS}" 'a sixth wrong password'
again=$(failure_buckets "${TEL_CTR}")
if [[ "${buckets}" =~ =5$ && "${again}" == "${buckets}" ]]; then
    pass "a 429 records nothing (${again})"
else
    fail "a 429 records nothing: expected '${buckets}' with five failures, got '${again}'"
fi
# The failures are on /data, so a restart forgives none of them
start_served "${TEL_CTR}" -v "${STATS_VOL}:/data" -e TELEMETRY=true -e "STATS_PASSWORD=${STATS_PASSWORD_VALUE}"
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq 429 "${STATUS}" 'after a restart: the right password'
probe "${TEL_CTR}" age-failures 901
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq 303 "${STATUS}" 'after age-failures 901: the right password'
# A second client, apart from this host: the server's own loopback
loopback_login http-request wrong-1
assert_eq 403 "${LOOPBACK_STATUS}" "from the server's own loopback, a second client: a wrong password"
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '127.0.0.1=1' "${buckets}" "the loopback's failure is counted apart from this host"
# One password is checked at a time, server-wide. While the probe holds
# login.php's lock, even the right password is turned away at once, and
# nothing is recorded.
loopback_login http-request-locked "${STATS_PASSWORD_VALUE}"
assert_eq 429 "${LOOPBACK_STATUS}" 'the right password while the probe holds the lock'
assert_contains 'Another login is being checked' 'the 429 says why' printf '%s' "${LOOPBACK_BODY}"
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '127.0.0.1=1' "${buckets}" 'turned away while busy, nothing is recorded'
# The probe is a process of its own. Between two of the server's threads it
# holds the same: with a hash at bcrypt cost 16 one check takes seconds, so
# login A is caught holding the lock in /proc/locks, read rather than probed
# with a lock of the test's own, and login B, sent then, is turned away.
probe "${TEL_CTR}" write-stats-hash "${STATS_PASSWORD_VALUE}" 16
curl -s -o /dev/null -w '%{http_code}' --max-time 120 --data-urlencode "password=${STATS_PASSWORD_VALUE}" \
    "${STATS_URL}?op=login" >"${WORK}/login-a" &
login_a=$!
holder=''
deadline=$(( SECONDS + 30 ))
while [[ -z "${holder}" && ! -s "${WORK}/login-a" ]] && (( SECONDS < deadline )); do
    holder=$(login_lock_holders)
done
assert_eq 'FLOCK WRITE 1' "${holder}" 'login A holds the lock: an flock(2) lock, taken by the server, PID 1'
loopback_login http-request wrong-2
assert_eq 429 "${LOOPBACK_STATUS}" "login B, sent meanwhile to another of the server's threads"
assert_contains 'Another login is being checked' 'login B: the 429 says why' printf '%s' "${LOOPBACK_BODY}"
wait "${login_a}"
login_a=$(<"${WORK}/login-a")
assert_eq 303 "${login_a}" 'login A, once checked: the right password'
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '127.0.0.1=1' "${buckets}" 'login B recorded nothing'
# PHP 8.5's default cost, as the bootstrap writes it
probe "${TEL_CTR}" write-stats-hash "${STATS_PASSWORD_VALUE}" 12
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq 303 "${STATUS}" 'with the hash back at cost 12, the lock free: the right password'

# A client already at its limit is answered before the lock is tried, so no
# flood from it ever holds the lock against anyone else
for i in 2 3 4 5; do
    loopback_login http-request "wrong-$(( i + 1 ))"
    assert_eq 403 "${LOOPBACK_STATUS}" "from the loopback: wrong password ${i} of 5"
done
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '127.0.0.1=5' "${buckets}" 'the loopback is at its limit'
loopback_login http-request-locked "${STATS_PASSWORD_VALUE}"
assert_eq 429 "${LOOPBACK_STATUS}" 'at its limit, while the probe holds the lock: the right password'
assert_contains 'Too many failed logins' 'at its limit, it is told so, not that a check is under way' \
    printf '%s' "${LOOPBACK_BODY}"
# A flood from it while the database's write lock is held: any request of
# the flood that took login.php's lock would sit inside it until the hold
# ends, and this host's login, sent meanwhile, would be turned away
"${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" hold-db-lock 3 >"${WORK}/db-hold" 2>&1 &
hold=$!
deadline=$(( SECONDS + 10 ))
until grep -qx held "${WORK}/db-hold" || (( SECONDS >= deadline )); do
    sleep 0.1
done
assert_contains held 'the database write lock is held' cat "${WORK}/db-hold"
for i in 1 2 3 4; do
    "${ENGINE}" exec "${TEL_CTR}" "${PHP_BIN}" php-cli "${PROBE}" http-request POST \
        'http://127.0.0.1:8080/results/stats.php?op=login' '' "password=${STATS_PASSWORD_VALUE}" \
        >"${WORK}/flood-${i}" 2>&1 &
done
# Until the flood is answered, or one of it holds the lock
deadline=$(( SECONDS + 10 ))
while (( SECONDS < deadline )); do
    answered=0
    for i in 1 2 3 4; do
        [[ -s "${WORK}/flood-${i}" ]] && answered=$(( answered + 1 ))
    done
    holder=$(login_lock_holders)
    (( answered == 4 )) || [[ -n "${holder}" ]] && break
done
stats_login "${STATS_URL}?op=login" "${STATS_PASSWORD_VALUE}" "${jar}"
assert_eq 303 "${STATUS}" 'this host, during a flood from the loopback: the right password'
wait
refused=0
for i in 1 2 3 4; do
    read -r first <"${WORK}/flood-${i}"
    if [[ "${first}" == 429 ]] && grep -q 'Too many failed logins' "${WORK}/flood-${i}"; then
        refused=$(( refused + 1 ))
    else
        sed "s/^/       | flood ${i}: /" "${WORK}/flood-${i}" >&2
    fi
done
assert_eq 4 "${refused}" "the flood: each told it is over its limit"
buckets=$(failure_buckets "${TEL_CTR}")
assert_eq '127.0.0.1=5' "${buckets}" 'the flood recorded nothing'
wait "${hold}" 2>/dev/null
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo '== 16. behind a trusted proxy, the key is the right-most address it does not trust; IPv6 counts per /64'
"${ENGINE}" network create --subnet "${PROXY_SUBNET}" "${PROXY_NET}" >/dev/null
"${ENGINE}" volume create "${PROXIED_VOL}" >/dev/null
"${ENGINE}" run -d --name "${PROXIED_CTR}" "${CONFINE[@]}" --network "${PROXY_NET}" --ip "${PROXIED_IP}" \
    -v "${PROXIED_VOL}:/data" -e TELEMETRY=true -e "STATS_PASSWORD=${STATS_PASSWORD_VALUE}" \
    -e "TRUSTED_PROXIES=${PROXY_SUBNET}" "${IMAGE}" >/dev/null
# The proxy is the image under test as well, so the suite needs no other
"${ENGINE}" run -d --name "${PROXY_CTR}" "${CONFINE[@]}" --network "${PROXY_NET}" --ip "${PROXY_IP}" \
    "${IMAGE}" >/dev/null
install_probe "${PROXY_CTR}"
deadline=$(( SECONDS + WAIT_FOR_PORT_SECONDS ))
while proxy_request GET / '' '' && [[ "${PROXY_STATUS}" != 200 ]]; do
    running=$("${ENGINE}" inspect --format '{{.State.Running}}' "${PROXIED_CTR}" 2>&1)
    if [[ "${running}" != true ]] || (( SECONDS >= deadline )); then
        fail "${PROXIED_CTR} never served its proxy: ${PROXY_STATUS}"
        "${ENGINE}" logs "${PROXIED_CTR}" 2>&1 | sed 's/^/       | /' >&2
        break
    fi
    sleep 1
done
assert_contains "trusted_proxies static ${PROXY_SUBNET}" "global.caddy trusts ${PROXY_SUBNET}" \
    generated_file "${PROXIED_CTR}" caddy/global.caddy
options=$(client_ip_options "${PROXIED_CTR}")
assert_eq "[{\"trusted_proxies\":{\"ranges\":[\"${PROXY_SUBNET}\"],\"source\":\"static\"},\"trusted_proxies_strict\":1,\"client_ip_headers\":[\"X-Forwarded-For\"]}]" \
    "${options}" 'the server trusts X-Forwarded-For from the proxy, read right to left'
for i in 1 2 3 4 5; do
    proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 203.0.113.7' "password=wrong-${i}"
    assert_eq 403 "${PROXY_STATUS}" "client A, 203.0.113.7: wrong password ${i} of 5"
done
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 203.0.113.7' "password=${STATS_PASSWORD_VALUE}"
assert_eq 429 "${PROXY_STATUS}" 'client A: the right password after five failures'
# IPv4 counts per address, and an IPv4-mapped IPv6 address as its IPv4
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 203.0.113.8' "password=${STATS_PASSWORD_VALUE}"
assert_eq 303 "${PROXY_STATUS}" "203.0.113.8, beside client A: the right password"
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: ::ffff:203.0.113.7' \
    "password=${STATS_PASSWORD_VALUE}"
assert_eq 429 "${PROXY_STATUS}" "client A's address written IPv4-mapped, ::ffff:203.0.113.7: the right password"
# The proxy appends the address it saw. Whatever the client wrote before it
# is the client's own claim.
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 203.0.113.7, 198.51.100.9' \
    "password=${STATS_PASSWORD_VALUE}"
assert_eq 303 "${PROXY_STATUS}" "client A's address claimed in front of 198.51.100.9: the right password"
# IPv6 counts per /64, what one subscriber is usually given
for i in 1 2 3 4 5; do
    proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 2001:db8::1' "password=wrong-${i}"
    assert_eq 403 "${PROXY_STATUS}" "client B, 2001:db8::1: wrong password ${i} of 5"
done
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 2001:db8::2' "password=${STATS_PASSWORD_VALUE}"
assert_eq 429 "${PROXY_STATUS}" "2001:db8::2, in client B's /64: the right password"
proxy_request POST '/results/stats.php?op=login' 'X-Forwarded-For: 2001:db8:0:1::1' \
    "password=${STATS_PASSWORD_VALUE}"
assert_eq 303 "${PROXY_STATUS}" '2001:db8:0:1::1, in the next /64: the right password'
buckets=$(failure_buckets "${PROXIED_CTR}")
assert_eq $'2001:db8::/64=5\n203.0.113.7=5' "${buckets}" "the failures are client A's and client B's /64's alone"
"${ENGINE}" rm -f "${PROXIED_CTR}" "${PROXY_CTR}" >/dev/null 2>&1
"${ENGINE}" network rm -f "${PROXY_NET}" >/dev/null 2>&1
# Only addresses and CIDR ranges, whatever else is set, and nothing else
# reaches the Caddyfile
assert_refused 'TRUSTED_PROXIES=not-a-cidr' \
    "TRUSTED_PROXIES holds 'not-a-cidr', which is not an IP address or CIDR range" -e TRUSTED_PROXIES=not-a-cidr
assert_refused 'TRUSTED_PROXIES with a /33' \
    "TRUSTED_PROXIES holds '10.0.0.0/33', which is not an IP address or CIDR range" \
    -e 'TRUSTED_PROXIES=10.0.0.0/8 10.0.0.0/33'
assert_refused 'TRUSTED_PROXIES that would close the servers block' \
    "TRUSTED_PROXIES holds '}', which is not an IP address or CIDR range" \
    -e $'TRUSTED_PROXIES=10.0.0.0/8 }\nadmin 0.0.0.0:2019'
assert_refused "TRUSTED_PROXIES with Caddy's own private_ranges" \
    "TRUSTED_PROXIES holds 'private_ranges', which is not an IP address or CIDR range" \
    -e TRUSTED_PROXIES=private_ranges
# What global.caddy holds is each address as PHP writes it, never the
# variable's own text
start_served "${TEL_CTR}" -e 'TRUSTED_PROXIES= 10.89.77.0/24,,2001:DB8::/32  192.0.2.1 '
line=$(generated_file "${TEL_CTR}" caddy/global.caddy | grep 'trusted_proxies static')
assert_eq $'\ttrusted_proxies static 10.89.77.0/24 2001:db8::/32 192.0.2.1' "${line}" \
    'a list with commas, spaces and capitals, as written to global.caddy'
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo '== 17. healthy: /healthz is 200 ok with telemetry off and on, and the engine runs the probe'
if [[ "${ENGINE}" != podman ]]; then
    # Podman builds and runs without it; Docker runs exactly this
    healthcheck=$("${ENGINE}" image inspect --format '{{json .Config.Healthcheck}}' "${IMAGE}" 2>&1)
    timings=$(jq -c '{Test, Interval, Timeout, StartPeriod, Retries}' 2>&1 <<<"${healthcheck}")
    assert_eq "{\"Test\":[\"CMD\",\"/usr/local/bin/frankenphp\",\"php-cli\",\"${HEALTH_PROBE}\"],\"Interval\":30000000000,\"Timeout\":10000000000,\"StartPeriod\":60000000000,\"Retries\":3}" \
        "${timings}" "the image's HEALTHCHECK: the probe in exec form, with no shell, at Baikal's timings"
    HEALTH_START_PERIOD=$(jq -r '(.StartPeriod // 0) / 1000000000 | floor' 2>/dev/null <<<"${healthcheck}")
    [[ "${HEALTH_START_PERIOD}" =~ ^[0-9]+$ ]] || HEALTH_START_PERIOD=0
fi
started=${SECONDS}
start_served "${TEL_CTR}" -p "${HEALTH_PORT}:8081" "${HEALTH_ARGS[@]}"
healthz
assert_eq '200 ok' "${HEALTHZ_STATUS} ${HEALTHZ_BODY}" 'telemetry off: GET :8081/healthz'
run_health_probe "${TEL_CTR}"
assert_eq 0 "${HEALTH_RC}" "telemetry off: ${HEALTH_PROBE} exits 0"
assert_eq '' "${HEALTH_OUT}" "telemetry off: ${HEALTH_PROBE} prints nothing"
assert_engine_healthy "${TEL_CTR}" "${started}" 'telemetry off'
# A fresh volume, and not one result posted: the database the check needs
# is there from the start
"${ENGINE}" volume create "${HEALTH_VOL}" >/dev/null
started=${SECONDS}
start_served "${TEL_CTR}" -p "${HEALTH_PORT}:8081" "${HEALTH_ARGS[@]}" -v "${HEALTH_VOL}:/data" -e TELEMETRY=true
table=$(results_table "${TEL_CTR}")
assert_eq "${EMPTY_RESULTS_TABLE}" "${table}" 'telemetry on, a fresh volume: no result has been recorded'
healthz
assert_eq '200 ok' "${HEALTHZ_STATUS} ${HEALTHZ_BODY}" 'telemetry on, a fresh volume: GET :8081/healthz'
run_health_probe "${TEL_CTR}"
assert_eq 0 "${HEALTH_RC}" "telemetry on, a fresh volume: ${HEALTH_PROBE} exits 0"
(( HEALTH_RC == 0 )) || printf '       | %s\n' "${HEALTH_OUT}" >&2
assert_engine_healthy "${TEL_CTR}" "${started}" 'telemetry on, a fresh volume'

echo '== 18. failure modes: each names its check in a 503, and the probe exits 1'
# The container from section 17, telemetry on. A database the server
# cannot write would take results and record none of them.
php_in "${TEL_CTR}" 'chmod("/data/telemetry.db", 0444);' >/dev/null
assert_health_fails 'with /data/telemetry.db read-only' database
php_in "${TEL_CTR}" 'chmod("/data/telemetry.db", 0644);' >/dev/null
healthz
assert_eq '200 ok' "${HEALTHZ_STATUS} ${HEALTHZ_BODY}" 'writable again: GET :8081/healthz'
probe "${TEL_CTR}" damage-db
assert_health_fails 'after damage-db' database
probe "${TEL_CTR}" remove-db
assert_health_fails 'after remove-db' database
# The check only reads. Asked, twice over, it made no database, so a
# missing one stays missing until upstream records a result.
gone=$(php_in "${TEL_CTR}" 'echo file_exists("/data/telemetry.db") ? "present" : "absent";')
assert_eq absent "${gone}" 'after remove-db and /healthz: /data/telemetry.db'
# PHP settings added from a mounted directory, as an operator might add
# them, reach the server too, and every one the image depends on fails the
# ini check. The bootstrap makes the database again, so ini alone fails.
php_on_volume "${HEALTH_VOL}" 'mkdir("/data/php.d"); file_put_contents("/data/php.d/zz-operator.ini",
    "allow_url_fopen = On\ndisplay_errors = On\nlog_errors = Off\n");'
start_served "${TEL_CTR}" -p "${HEALTH_PORT}:8081" "${HEALTH_ARGS[@]}" -v "${HEALTH_VOL}:/data" -e TELEMETRY=true \
    -e PHP_INI_SCAN_DIR=:/data/php.d
assert_health_fails 'with allow_url_fopen and display_errors on, and log_errors off' ini
assert_eq 'ini: allow_url_fopen is on, display_errors is on, log_errors is off' "${HEALTHZ_BODY}" \
    'the three settings, on one line, and no other check failed'
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo '== 19. the health port serves nothing else, and :8080 has no /healthz'
start_served "${TEL_CTR}" -p "${HEALTH_PORT}:8081"
for path in / /backend/getIP.php /index.php; do
    assert_status 404 "GET :8081${path}" "${HEALTH_BASE}${path}"
done
assert_status 404 'GET :8080/healthz' "${TEL_BASE}/healthz"
"${ENGINE}" rm -f "${TEL_CTR}" >/dev/null 2>&1

echo
if (( FAILURES == 0 )); then
    echo "acceptance: all checks passed"
    exit 0
fi
echo "acceptance: ${FAILURES} check(s) failed" >&2
exit 1
