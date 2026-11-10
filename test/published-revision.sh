#!/usr/bin/env bash
# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>
set -euo pipefail

IMAGE="${1:?image whose published revision to print}"

# Prints the commit the image was built from, read off the
# org.opencontainers.image.revision annotation that publish writes on the
# index. Prints nothing when the registry says there is no such image, so a
# first publish has nothing to compare with. Anything else is an error: a
# revision nobody can read must never be mistaken for one that is absent.

# stderr is kept apart from the index: a warning mixed into the JSON would
# stop every publish on a jq parse error.
errors=$(mktemp)
trap 'rm -f "${errors}"' EXIT
if ! index=$(docker buildx imagetools inspect --raw "${IMAGE}" 2>"${errors}"); then
    # Absent only when the error is about this image. A missing tag reads
    # "ERROR: <image>: not found". ghcr.io answers the token request for a
    # missing repository with 403 Forbidden rather than a 404, and would
    # answer the same for a private one, which this image never is.
    repository="${IMAGE#*/}"
    repository="${repository%%[:@]*}"
    if grep -qxF "ERROR: ${IMAGE}: not found" "${errors}" \
            || { grep -qF "scope=repository%3A${repository//\//%2F}%3Apull" "${errors}" \
                 && grep -qF '403 Forbidden' "${errors}"; }; then
        exit 0
    fi
    cat "${errors}" >&2
    echo "could not read ${IMAGE}, so nothing says which commit it was built from" >&2
    exit 1
fi

revision=$(jq -r '.annotations["org.opencontainers.image.revision"] // empty' <<<"${index}")
if [[ ! "${revision}" =~ ^[0-9a-f]{40}$ ]]; then
    echo "${IMAGE} carries no revision annotation, only '${revision}', so nothing says which commit it was built from" >&2
    exit 1
fi
echo "${revision}"
