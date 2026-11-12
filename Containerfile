# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

# Renovate moves these two in one PR; its regex needs them adjacent and in
# this order. The release has no assets, only GitHub's generated archives,
# which are not byte-stable, so the fetch stage checks out the commit and
# asserts the tag still names it.
ARG LIBRESPEED_VERSION=6.3.1
ARG LIBRESPEED_COMMIT=a820d7cdf1fea13592809966ba845e68efff7ef9
ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian13:nonroot@sha256:cb87acc574abe719b6e5c547384defa2f7b1f24af9605d3f618cf852495d8be2

FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS fetch
ARG LIBRESPEED_VERSION
ARG LIBRESPEED_COMMIT
# git checks a fetch by commit against its hash. The tag is asked for
# separately, peeled if it is ever annotated, so a tag moved upstream fails
# the build instead of shipping a commit no release names. The fetch and
# the tag lookup each get three retries, 10s, 30s and 90s apart, so one
# dropped connection does not cost the daily slot.
# DL3008 asks for apt versions to be pinned. The base is pinned by digest, and
# git and its certificates only fetch here: nothing from this stage but the
# checked-out files reaches the image.
# hadolint ignore=DL3008
RUN set -eux; \
    retry() { \
        for delay in 10 30 90; do "$@" && return 0; sleep "${delay}"; done; \
        "$@"; \
    }; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates git; \
    rm -rf /var/lib/apt/lists/*; \
    git init -q /src; \
    retry git -C /src fetch -q --depth 1 \
        https://github.com/librespeed/speedtest.git "${LIBRESPEED_COMMIT}"; \
    git -C /src checkout -q --detach FETCH_HEAD; \
    test "$(git -C /src rev-parse HEAD)" = "${LIBRESPEED_COMMIT}"; \
    retry git ls-remote https://github.com/librespeed/speedtest.git \
        "refs/tags/v${LIBRESPEED_VERSION}" "refs/tags/v${LIBRESPEED_VERSION}^{}" >/tmp/tag; \
    tagged=$(awk -v tag="refs/tags/v${LIBRESPEED_VERSION}" \
        '$2 == tag "^{}" { peeled = $1 } $2 == tag { plain = $1 } END { print (peeled != "" ? peeled : plain) }' /tmp/tag); \
    if [ "${tagged}" != "${LIBRESPEED_COMMIT}" ]; then \
        echo "tag v${LIBRESPEED_VERSION} names '${tagged}', the Containerfile pins ${LIBRESPEED_COMMIT}" >&2; \
        exit 1; \
    fi
COPY test/upstream-layout.sh /tmp/upstream-layout.sh
RUN /tmp/upstream-layout.sh /src
# Only what is served, plus the licence. index-modern.html and settings.json
# are templates the bootstrap renders into /tmp/librespeed on every start,
# and are never served themselves. Nor is frontend/index.html, upstream's raw
# copy of that page with its placeholders still in, or
# background-original.jpeg, which nothing references. The symlinks dangle
# until the bootstrap runs: everything that changes per start lives on the
# /tmp tmpfs, and the obfuscation salt on /data.
RUN set -eux; \
    out=/out/librespeed; \
    mkdir -p "${out}/backend" "${out}/results" /out/librespeed-templates /data; \
    cp /src/speedtest.js /src/speedtest_worker.js /src/stability.html /src/stability_worker.js \
        /src/favicon.ico /src/manifest.webmanifest /src/LICENSE "${out}/"; \
    cp -R /src/images "${out}/images"; \
    cp -R /src/frontend "${out}/frontend"; \
    rm -r "${out}/frontend/README.md" "${out}/frontend/screenshots" \
        "${out}/frontend/index.html" "${out}/frontend/images/background-original.jpeg"; \
    cp /src/backend/empty.php /src/backend/garbage.php /src/backend/getIP.php \
        /src/backend/getIP_util.php /src/backend/getIP_ipInfo_apikey.php \
        /src/backend/geoip2.phar /src/backend/country_asn.mmdb "${out}/backend/"; \
    cp /src/results/telemetry.php /src/results/telemetry_db.php /src/results/json.php \
        /src/results/index.php /src/results/stats.php /src/results/idObfuscation.php \
        /src/results/OpenSans-*.ttf "${out}/results/"; \
    cp /src/index-modern.html /src/settings.json /out/librespeed-templates/; \
    ln -s /tmp/librespeed/index.html "${out}/index.html"; \
    ln -s /tmp/librespeed/settings.json "${out}/settings.json"; \
    ln -s /tmp/librespeed/server-list.json "${out}/server-list.json"; \
    ln -s /tmp/librespeed/server-list.json "${out}/servers.json"; \
    ln -s /tmp/librespeed/telemetry_settings.php "${out}/results/telemetry_settings.php"; \
    ln -s /data/idObfuscation_salt.php "${out}/results/idObfuscation_salt.php"
# The stats login and the health endpoint are the image's own, not
# upstream's. They pass through here only to get exact modes: COPY --chmod
# gives the directory it creates the file's mode, 0644, and then no one can
# enter it.
COPY rootfs/usr/local/share/librespeed-login/login.php /tmp/login.php
COPY rootfs/usr/local/share/librespeed-health/index.php /tmp/health.php
RUN install -D -m 0644 /tmp/login.php /out/librespeed-login/login.php \
    && install -D -m 0644 /tmp/health.php /out/librespeed-health/index.php

FROM ${RUNTIME_IMAGE}
ARG LIBRESPEED_VERSION

# The binary divbast/frankenphp builds for this image, by digest. CI
# refuses it unless that repository's own build on main attested it.
COPY --from=ghcr.io/divbast/frankenphp:librespeed@sha256:c6cd70db2268e9f9c69629e35479d5853025dd838281ba3234555643eda1d15a --chmod=0755 /frankenphp /usr/local/bin/frankenphp
COPY --from=fetch --chown=root:root /out/librespeed /var/www/librespeed
COPY --from=fetch --chown=root:root /out/librespeed-templates /usr/local/share/librespeed-templates
# The stats login, outside the served tree. Caddy runs it only through the
# rewrite site.caddy holds when STATS_PASSWORD is set.
COPY --from=fetch --chown=root:root /out/librespeed-login /usr/local/share/librespeed-login
# /healthz, which the Caddyfile serves on :8081 alone
COPY --from=fetch --chown=root:root /out/librespeed-health /usr/local/share/librespeed-health
COPY --chown=root:root caddy/Caddyfile /etc/frankenphp/Caddyfile
# The binary scans /etc/frankenphp/php.d, so this applies to the server and to
# every php-cli run alike
COPY --chown=root:root php/librespeed.ini /etc/frankenphp/php.d/librespeed.ini
COPY --from=fetch --chown=65532:65532 /data /data

ENV XDG_DATA_HOME=/tmp \
    XDG_CONFIG_HOME=/tmp

COPY --chown=root:root --chmod=0755 rootfs/usr/local/bin/librespeed-bootstrap /usr/local/bin/librespeed-bootstrap
COPY --chown=root:root --chmod=0755 rootfs/usr/local/bin/librespeed-health /usr/local/bin/librespeed-health

# The licences: this repository's, LibreSpeed's, IPinfo's for the ISP
# database, and Open Sans's and Inter's for the fonts. The README's
# "Licences in the image" says which file carries which.
LABEL org.opencontainers.image.source="https://github.com/divbast/librespeed-oci-image" \
    org.opencontainers.image.url="https://github.com/divbast/librespeed-oci-image" \
    org.opencontainers.image.title="LibreSpeed" \
    org.opencontainers.image.version="${LIBRESPEED_VERSION}" \
    org.opencontainers.image.licenses="BSD-2-Clause AND LGPL-3.0-or-later AND CC-BY-SA-4.0 AND Apache-2.0 AND OFL-1.1"

EXPOSE 8080 8081
# Exec form: the shell form would need /bin/sh, which the image lacks.
# --timeout must clear librespeed-health's own budget: it allows each of its
# two exchanges 4 s and runs them one after the other, so a listener that
# accepts and never answers costs 8 s. A shorter timeout kills the probe
# mid-flight and records a bare engine timeout instead of the probe's own
# line, and with no shell and no access log on :8081 that line is the only
# diagnostic there is. Podman reads no HEALTHCHECK from an image published
# as an OCI index: its users pass the same array as --health-cmd.
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD ["/usr/local/bin/frankenphp", "php-cli", "/usr/local/bin/librespeed-health"]
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/frankenphp", "php-cli", "/usr/local/bin/librespeed-bootstrap"]
