#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

TREE="${1:?path to the LibreSpeed checkout}"
FAILED=0

# The image serves upstream's files unedited, and the bootstrap rewrites a
# copy of index-modern.html by finding upstream's own text in it. Every file
# and every string either relies on is named here, so a release that moves or
# rewords one fails the build that fetches it, instead of shipping a page the
# bootstrap could not fill in or a route that points at nothing.
fail() {
    echo "ERROR: ${1}" >&2
    FAILED=1
}

FILES=(
    index-modern.html
    settings.json
    speedtest.js
    speedtest_worker.js
    stability.html
    stability_worker.js
    backend/empty.php
    backend/garbage.php
    backend/getIP.php
    backend/getIP_util.php
    backend/getIP_ipInfo_apikey.php
    backend/geoip2.phar
    backend/country_asn.mmdb
    results/telemetry.php
    results/telemetry_db.php
    results/json.php
    results/index.php
    results/stats.php
    results/idObfuscation.php
)

for file in "${FILES[@]}"; do
    [[ -f "${TREE}/${file}" ]] || fail "${file} is missing from ${TREE}."
done
[[ -d "${TREE}/frontend" ]] || fail "frontend/ is missing from ${TREE}."

# contains <file> <marker>: the file holds the marker as a literal string.
contains() {
    local file="${1}" marker="${2}"
    [[ -f "${TREE}/${file}" ]] || return 0 # already reported as missing
    grep -qF -- "${marker}" "${TREE}/${file}" \
        || fail "${file} no longer contains the marker: ${marker}"
}

contains index-modern.html '<title>LibreSpeed - Free and Open Source Speedtest</title>'
contains index-modern.html '<h1>Free and Open Source Speedtest.</h1>'
contains index-modern.html '<p class="tagline">No Flash, No Java, No Websockets, No Bullsh*t</p>'
contains index-modern.html 'mailto:PUT@YOUR_EMAIL.HERE'
contains index-modern.html 'TO BE FILLED BY DEVELOPER'
contains index-modern.html 'var SPEEDTEST_SERVERS = "server-list.json";'
contains stability.html 'var SPEEDTEST_SERVERS = "server-list.json";'
contains results/stats.php "\$_SESSION['logged'] === true"
contains results/stats.php "\$_POST['password'] === \$stats_password"
contains results/idObfuscation.php "__DIR__.'/idObfuscation_salt.php'"

# getIP.php calls ipinfo.io whenever this file sets a key. The image allows
# no outbound connection, so upstream's own default must stay empty.
apikey=backend/getIP_ipInfo_apikey.php
if [[ -f "${TREE}/${apikey}" ]]; then
    grep -qxF "\$IPINFO_APIKEY = '';" "${TREE}/${apikey}" \
        || fail "${apikey} no longer sets the marker: \$IPINFO_APIKEY = '';"
    if grep -qE "\\\$IPINFO_APIKEY *= *['\"][^'\"]" "${TREE}/${apikey}"; then
        fail "${apikey} sets a non-empty \$IPINFO_APIKEY."
    fi
fi

if ((FAILED)); then
    echo "Upstream's layout changed. Adapt the image to it before taking this release." >&2
    exit 1
fi
echo "every file and marker the image relies on is in ${TREE}"
