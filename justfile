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
    # Piped with -r so an empty match is a pass
    git ls-files '*.sh' | xargs -r shellcheck
    reuse lint
