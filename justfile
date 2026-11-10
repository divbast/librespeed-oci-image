# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

set shell := ["mise", "exec", "--", "bash", "-euo", "pipefail", "-c"]

ENGINE := env_var_or_default("ENGINE", "docker")
IMAGE := "localhost/librespeed:dev"
ARCHIVE := env_var_or_default("ARCHIVE", "/tmp/librespeed-image.tar")
LAYOUT := ARCHIVE + ".layout"
REPORT := ARCHIVE + ".report.json"
SARIF := ARCHIVE + ".sarif"
PUBLISHED := env_var_or_default("PUBLISHED", "ghcr.io/divbast/librespeed:latest")
BASELINE := ARCHIVE + ".published.json"

# Tool versions live in mise.toml

default:
    @just --list

setup:
    #!/usr/bin/env bash
    # This is the one recipe that must not run through `mise exec` shell 
    set -euo pipefail
    mise install
    composer install

# Build the image locally
build:
    # Docker only auto-discovers `Dockerfile`. Podman finds either
    # gh's token, when gh is logged in, reaches spc as a secret so its GitHub
    # API calls escape the unauthenticated rate limit. It never reaches a layer.
    token="${GITHUB_TOKEN:-$(gh auth token 2>/dev/null || true)}"; \
    if [[ -n "${token}" ]]; then \
        GITHUB_TOKEN="${token}" {{ ENGINE }} build --secret id=github_token,env=GITHUB_TOKEN \
            -f Containerfile -t {{ IMAGE }} .; \
    else \
        echo "gh is not logged in: building without a GitHub token" >&2; \
        {{ ENGINE }} build -f Containerfile -t {{ IMAGE }} .; \
    fi

acceptance:
    ENGINE={{ ENGINE }} IMAGE={{ IMAGE }} ./test/acceptance.sh

# Build the image, then run the acceptance suite against it
test: build acceptance

# Write the image
archive:
    {{ ENGINE }} image save -o {{ ARCHIVE }} {{ IMAGE }}

# Fail if the archive holds the GitHub token the build was given
token-check:
    token="${GITHUB_TOKEN:-$(gh auth token 2>/dev/null || true)}"; \
    if [[ -n "${token}" ]]; then \
        GITHUB_TOKEN="${token}" ./test/token-leak.sh {{ ARCHIVE }}; \
    else \
        echo "gh is not logged in: the build had no token to leak"; \
    fi

# Fail on any HIGH or CRITICAL vulnerability the published image does not have.
# While nothing is published, every finding is reported and none fails
scan:
    rm -rf {{ LAYOUT }} && mkdir {{ LAYOUT }} && tar xf {{ ARCHIVE }} -C {{ LAYOUT }}
    ./test/scan.sh {{ LAYOUT }} {{ REPORT }} {{ PUBLISHED }} {{ BASELINE }} {{ SARIF }}

# Verify every action is a SHA. The workflow recipes skip until .github/workflows
# exists
actions-check:
    if [[ -d .github/workflows ]]; then GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}" pinact run --verify --check; else echo "no workflows yet"; fi

# Rewrite tag references to the SHA
actions-pin:
    GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}" pinact run

# Move every action to latest release
actions-update:
    GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}" pinact run --update

workflow-audit:
    if [[ -d .github/workflows ]]; then GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token)}" zizmor --persona=auditor .github/workflows; else echo "no workflows yet"; fi

lint: actions-check workflow-audit
    hadolint Containerfile
    # BuildKit's own linter
    docker build --check -f Containerfile .
    # Piped with -r so an empty match is a pass
    git ls-files '*.sh' | xargs -r shellcheck
    reuse lint
    vendor/bin/phpstan analyse --no-progress
    # The README's Env Vars table names exactly the variables the image reads
    ./test/env-docs.sh

ci: lint test archive token-check scan
