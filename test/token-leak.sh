#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

ARCHIVE="${1:?path to the image archive}"
: "${GITHUB_TOKEN:?the token the build was given, in the environment}"

# spc gets a GitHub token through a secret mount, which never reaches a layer.
# An ARG or ENV carrying it would, and so would anything that writes it to a
# file. This reads every blob in the archive, the layers decompressed, so the
# config, its history and any attestation are read too, and fails if one
# holds the token. The token stays off every command line: grep reads it as
# a pattern file.
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT
printf '%s\n' "${GITHUB_TOKEN}" >"${work}/pattern"
mkdir "${work}/archive"
tar -xf "${ARCHIVE}" -C "${work}/archive"

find "${work}/archive" -type f -print0 >"${work}/blobs"

FOUND=0
SCANNED=0
while IFS= read -r -d '' blob; do
    name="${blob#"${work}/archive/"}"
    magic=$(head -c 4 "${blob}" | od -An -tx1 | tr -d ' \n')
    case "${magic}" in
    1f8b*) reader=(gzip -dc) ;;
    28b52ffd) reader=(zstd -dcq) ;;
    *) reader=(cat) ;;
    esac
    # No grep -q: it would stop reading early and the reader would die of
    # SIGPIPE, indistinguishable from a blob it could not read.
    set +e
    "${reader[@]}" "${blob}" | grep -aFf "${work}/pattern" >/dev/null
    status=("${PIPESTATUS[@]}")
    set -e
    if ((status[0] != 0)); then
        echo "ERROR: could not read ${name} with ${reader[*]}; a blob this check cannot read is not a blob it has cleared." >&2
        exit 2
    fi
    case "${status[1]}" in
    0)
        echo "ERROR: ${name} contains the GitHub token the build was given." >&2
        FOUND=1
        ;;
    1) ;;
    *)
        echo "ERROR: grep failed on ${name}." >&2
        exit 2
        ;;
    esac
    SCANNED=$((SCANNED + 1))
done <"${work}/blobs"

if ((SCANNED == 0)); then
    echo "ERROR: ${ARCHIVE} holds no files to read." >&2
    exit 2
fi
if ((FOUND)); then
    echo "The token must reach the build only as a secret mount, never as an ARG or ENV." >&2
    exit 1
fi
echo "no trace of the token in any of the ${SCANNED} files in ${ARCHIVE}"
