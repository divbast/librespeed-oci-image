#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

# Usage: upstream-layout.sh TREE
#
# Fails unless the LibreSpeed checkout at TREE holds every path and every
# string the image relies on. The image serves upstream's files unedited,
# and the bootstrap rewrites a copy of index-modern.html by finding
# upstream's own text in it, so a release that moves or rewords one must
# fail the build that fetches it, rather than ship a page the bootstrap
# could not fill in, a route that points at nothing, or a licences label
# that names a file no longer there.
#
# The Containerfile's fetch stage runs it over its checkout.
# test/upstreamlayout runs it over fixtures, and fails when the fetch stage
# copies a path out of the checkout that this script does not name.
#
# Exit status: 0 when everything is there; 1 when something moved or
# changed, with an ERROR line on stderr for each; 2 when TREE is not one
# directory.
set -euo pipefail

if (($# != 1)) || [[ ! -d "${1}" ]]; then
	echo "usage: ${0} TREE, where TREE is the directory LibreSpeed is checked out in" >&2
	exit 2
fi
TREE="${1}"
FAILED=0

fail() {
	echo "ERROR: ${1}" >&2
	FAILED=1
}

FILES=(
	# The templates the bootstrap renders on every start
	index-modern.html
	settings.json
	# The speed test's client, the stability test, and what they load
	speedtest.js
	speedtest_worker.js
	stability.html
	stability_worker.js
	favicon.ico
	manifest.webmanifest
	# LibreSpeed's licence, which ships beside them
	LICENSE
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
	# The fonts the image's licences label names: Open Sans, which the
	# share image is drawn in, and Inter, inside frontend/, which the page
	# is set in
	results/OpenSans-Light.ttf
	results/OpenSans-Semibold.ttf
	frontend/fonts/Inter-latin.woff2
	frontend/fonts/Inter-latin-ext.woff2
)
# The directories the fetch stage copies whole
DIRS=(
	frontend
	images
)

for file in "${FILES[@]}"; do
	[[ -f "${TREE}/${file}" ]] || fail "${file} is missing from ${TREE}."
done
for dir in "${DIRS[@]}"; do
	[[ -d "${TREE}/${dir}" ]] || fail "${dir}/ is missing from ${TREE}."
done

# copies <file> <marker>: prints how many times the file holds the marker,
# as a literal string. Prints nothing for a file already reported missing.
copies() {
	local file="${1}" marker="${2}" found
	[[ -f "${TREE}/${file}" ]] || return 0
	# grep exits 1 when it finds none, which is an answer here: 0
	found=$(grep -oF -- "${marker}" "${TREE}/${file}" || true)
	if [[ -z "${found}" ]]; then
		echo 0
	else
		wc -l <<<"${found}" | tr -d ' '
	fi
}

# contains <file> <marker>: the file holds the marker.
contains() {
	local count
	count=$(copies "${1}" "${2}")
	if [[ "${count}" == 0 ]]; then
		fail "${1} no longer contains the marker: ${2}"
	fi
}

# once <file> <marker>: the file holds the marker exactly once, as
# renderIndex requires of each marker it replaces before it will start.
once() {
	local count
	count=$(copies "${1}" "${2}")
	if [[ "${count}" == 0 ]]; then
		fail "${1} no longer contains the marker: ${2}"
	elif [[ -n "${count}" && "${count}" != 1 ]]; then
		fail "${1} holds ${count} copies of the marker: ${2}, where the bootstrap replaces exactly one"
	fi
}

once index-modern.html '<title>LibreSpeed - Free and Open Source Speedtest</title>'
once index-modern.html '<h1>Free and Open Source Speedtest.</h1>'
once index-modern.html '<p class="tagline">No Flash, No Java, No Websockets, No Bullsh*t</p>'
once index-modern.html 'mailto:PUT@YOUR_EMAIL.HERE'
once index-modern.html 'TO BE FILLED BY DEVELOPER'
contains index-modern.html 'var SPEEDTEST_SERVERS = "server-list.json";'
contains stability.html 'var SPEEDTEST_SERVERS = "server-list.json";'
contains results/stats.php "\$_SESSION['logged'] === true"
contains results/stats.php "\$_POST['password'] === \$stats_password"
contains results/idObfuscation.php "__DIR__.'/idObfuscation_salt.php'"

# getIP.php calls ipinfo.io whenever this file sets a key, and the image
# allows no outbound connection. So the file must be upstream's own, which
# sets the key empty and does nothing else: past its blank lines and line
# comments, exactly these two lines. A key from anywhere, a literal or a
# call, fails.
apikey=backend/getIP_ipInfo_apikey.php
if [[ -f "${TREE}/${apikey}" ]]; then
	code=$(grep -vxE '[[:space:]]*(//.*)?' "${TREE}/${apikey}" || true)
	if [[ "${code}" != $'<?php\n$IPINFO_APIKEY = \'\';' ]]; then
		fail "${apikey} does more than set \$IPINFO_APIKEY = '';, as upstream's own does, so getIP.php could call ipinfo.io."
	fi
fi

if ((FAILED)); then
	echo "Upstream's layout changed. Adapt the image to it before taking this release." >&2
	exit 1
fi
echo "every file and marker the image relies on is in ${TREE}"
