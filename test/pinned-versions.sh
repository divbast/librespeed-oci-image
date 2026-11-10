#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

BIN="${1:?path to the frankenphp binary}"
PHP_VERSION="${2:?PHP version the Containerfile pins}"
CADDY_VERSION="${3:?Caddy version the Containerfile pins}"
CBROTLI_VERSION="${4:?caddy-cbrotli version the Containerfile pins}"
MERCURE_VERSION="${5:?mercure/caddy version the Containerfile pins}"
VULCAIN_VERSION="${6:?vulcain/caddy version the Containerfile pins}"

VERSION=$("${BIN}" version)
INFO=$("${BIN}" build-info)
FAILED=0

# A pin only means something if the binary agrees with it. Minimum version
# selection lifts a module above its pin whenever something else in the graph
# requires more, and the build then ships a version no ARG names.
check() {
    local what="${1}" expected="${2}" found="${3}"
    if [[ "${found}" != "${expected}" ]]; then
        echo "ERROR: the binary has ${what} ${found:-(absent)}, the Containerfile pins ${expected}." >&2
        FAILED=1
    fi
}

# build-info prints one "dep<TAB>module<TAB>version<TAB>sum" line per module.
dep() {
    awk -F'\t' -v module="${1}" '$1 == "dep" && $2 == module { print $3 }' <<<"${INFO}"
}

php=$(grep -oE 'PHP [0-9]+\.[0-9]+\.[0-9]+' <<<"${VERSION}" | cut -d' ' -f2) || true
check PHP "${PHP_VERSION}" "${php}"

caddy=$(dep github.com/caddyserver/caddy/v2)
check github.com/caddyserver/caddy/v2 "${CADDY_VERSION}" "${caddy}"

cbrotli=$(dep github.com/dunglas/caddy-cbrotli)
check github.com/dunglas/caddy-cbrotli "${CBROTLI_VERSION}" "${cbrotli}"

mercure=$(dep github.com/dunglas/mercure/caddy)
check github.com/dunglas/mercure/caddy "${MERCURE_VERSION}" "${mercure}"

vulcain=$(dep github.com/dunglas/vulcain/caddy)
check github.com/dunglas/vulcain/caddy "${VULCAIN_VERSION}" "${vulcain}"

if (( FAILED )); then
    echo "Raise each ARG named above to the version the binary reports, or" >&2
    echo "find what in the module graph now requires more." >&2
    exit 1
fi
echo "binary matches every pin: PHP ${php}, caddy ${caddy}, cbrotli ${cbrotli}, mercure ${mercure}, vulcain ${vulcain}"
