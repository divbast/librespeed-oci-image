# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

set shell := ["mise", "exec", "--", "bash", "-euo", "pipefail", "-c"]

# What divbast/ci's app recipes act on: the engine, the image the build
# writes and the acceptance suite runs, where archive writes it and scan
# reads it, and the published image scan compares it with. IMAGE is a
# plain value, as ci's README has every app write it, so the environment
# cannot change it: `just IMAGE=<image> acceptance` tests another image.
ENGINE := env_var_or_default("ENGINE", "docker")
IMAGE := "localhost/librespeed:dev"
ARCHIVE := env_var_or_default("ARCHIVE", "/tmp/librespeed-image.tar")
PUBLISHED := env_var_or_default("PUBLISHED", "ghcr.io/divbast/librespeed:latest")

# build, acceptance, test, archive, scan and app-lint come from divbast/ci's
# app.just, and the other lint recipes from its common.just. Each is run
# from a recipe's body, never named as a dependency, so `just setup` still
# runs before .ci exists.
import? '.ci/just/common.just'
import? '.ci/just/app.just'

# Tool versions live in mise.toml

default:
    @just --list

# Fetch divbast/ci at the commit the workflows pin, then install the toolchain
setup: ci-checkout
    #!/usr/bin/env bash
    # Not through the mise exec shell: this is what installs what it runs
    set -euo pipefail
    mise install
    composer install

# Fetch divbast/ci at the commit the workflows pin, into .ci, and nothing else
ci-checkout:
    #!/usr/bin/env bash
    # Not through the mise exec shell, which would first install what is missing
    set -euo pipefail
    # The one commit the workflows call divbast/ci at, spelt in any case
    uses="^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[\"']?divbast/ci/[^@[:space:]\"']+@[^[:space:]\"'#]+"
    sha=$(grep -hoiE "${uses}" .github/workflows/*.y*ml | sed 's/.*@//' | sort -u | paste -sd ' ' - || true)
    if [[ ! "${sha}" =~ ^[0-9a-f]{40}$ ]]; then
        echo "ci-checkout: the workflows must call divbast/ci at one full commit SHA, not at: ${sha:-none}" >&2
        exit 1
    elif [[ "$(git -C .ci rev-parse HEAD 2>/dev/null || true)" == "${sha}" ]]; then
        echo "ci-checkout: .ci is already at ${sha}"
        exit 0
    fi
    changed=$([[ ! -e .ci/.git ]] || git -C .ci status --porcelain)
    if [[ -n "${changed}" ]]; then
        printf 'ci-checkout: moving .ci to %s would carry its own changes across, or stop on them; save or discard them first:\n%s\n' "${sha}" "${changed}" >&2
        exit 1
    fi
    git init -q .ci
    git -C .ci fetch -q --depth 1 https://github.com/divbast/ci "${sha}"
    git -C .ci checkout -q FETCH_HEAD && rm -rf .ci/bin

# Every lint gate. Needs a running Docker daemon, for docker build --check
lint:
    {{ just_executable() }} ci-fresh actions-check workflow-audit actionlint shell-lint go-lint go-test reuse-lint app-lint
    vendor/bin/phpstan analyse --no-progress

# Every gate. Run before committing
ci:
    {{ just_executable() }} lint test archive scan
