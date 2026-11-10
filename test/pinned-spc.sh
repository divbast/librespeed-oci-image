#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

DIST="${1:?the dist directory build-static.sh builds in}"
SRC="${2:?the static-php-cli checkout the Containerfile pins}"
SPC_COMMIT="${3:?static-php-cli commit the Containerfile pins}"
FAILED=0

# build-static.sh downloads a nightly spc into dist/spc unless one is already
# executable there, and clones static-php-cli's v3 branch into
# dist/static-php-cli if that download fails. Either would build the binary
# with an spc no pin names. The wrapper the Containerfile puts at dist/spc
# records each subcommand it runs, so a craft run on that record is proof the
# pinned spc did the build, not merely that a file was in place.
fail() {
    echo "ERROR: ${1}" >&2
    FAILED=1
}

if ! grep -qxF "exec php ${SRC}/bin/spc \"\$@\"" "${DIST}/spc" 2>/dev/null; then
    fail "${DIST}/spc is not the wrapper around ${SRC}; build-static.sh fetched its own spc."
fi
if ! grep -qx craft "${DIST}/spc.calls" 2>/dev/null; then
    fail "the wrapper never ran spc craft; something else built the binary."
fi
if [[ -e "${DIST}/static-php-cli" ]]; then
    fail "build-static.sh cloned static-php-cli's v3 branch into ${DIST}/static-php-cli."
fi
head=$(git -C "${SRC}" rev-parse HEAD 2>/dev/null) || true
changed=$(git -C "${SRC}" status --porcelain --untracked-files=no 2>/dev/null) || true
if [[ "${head}" != "${SPC_COMMIT}" ]]; then
    fail "${SRC} is at ${head:-(no checkout)}, the Containerfile pins ${SPC_COMMIT}."
elif [[ -n "${changed}" ]]; then
    fail "${SRC} has changes to tracked files on top of ${SPC_COMMIT}."
fi

if ((FAILED)); then
    exit 1
fi
echo "the binary was built by static-php-cli ${SPC_COMMIT}"
