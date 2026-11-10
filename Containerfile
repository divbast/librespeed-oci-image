# SPDX-License-Identifier: BSD-2-Clause
# SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

# Only the extensions LibreSpeed and the bootstrap need. pdo_sqlite cannot be
# built without sqlite3, so the binary carries both; test/extensions.sh fails
# the build if this list and the binary disagree.
ARG PHP_EXTENSIONS="ctype,filter,openssl,phar,pdo,pdo_sqlite,sqlite3,session,gd,zlib,pcntl,posix,opcache"
# watcher is FrankenPHP's file watcher. freetype and libpng are what gd needs
# to draw the share image's text and write it as a PNG. spc builds both
# already, freetype as one of gd's suggestions and libpng as a dependency, so
# naming them here keeps the share image working if spc ever stops.
ARG PHP_EXTENSION_LIBS="watcher,freetype,libpng"
ARG BUILDER_IMAGE=dunglas/frankenphp:static-builder-gnu@sha256:4dcdd53971ba1f373c3eb90e393dd3944687938c5c03494ab684921594905fc7
# The binary's own roots. Unset, build-static.sh takes the newest PHP 8.5 from
# php.net and xcaddy the newest Caddy and plugins, so two builds of one commit
# ship different binaries. test/pinned-versions.sh checks the binary agrees.
ARG PHP_VERSION=8.5.12
ARG CADDY_VERSION=v2.11.9
ARG CBROTLI_VERSION=v1.0.1
ARG MERCURE_VERSION=v1.1.6
ARG VULCAIN_VERSION=v1.4.6
# static-php-cli, which build-static.sh would otherwise download as an
# unchecked nightly binary on every build. Its v3 branch has no tags, so
# Renovate follows the branch head by commit. test/pinned-spc.sh checks it ran.
ARG SPC_COMMIT=ff04d4af9ba44f6c194239d733e0529cc6ddecea
# Renovate moves these two in one PR; its regex needs them adjacent and in
# this order. The release has no assets, only GitHub's generated archives,
# which are not byte-stable, so the fetch stage checks out the commit and
# asserts the tag still names it.
ARG LIBRESPEED_VERSION=6.3.1
ARG LIBRESPEED_COMMIT=a820d7cdf1fea13592809966ba845e68efff7ef9
ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian13:nonroot@sha256:cc66d222f0a7ec996b78a99fe76a969bc80b539a5016ad866c14d1949bc6d0ab

FROM ${BUILDER_IMAGE} AS frankenphp-build
ARG PHP_EXTENSIONS
ARG PHP_EXTENSION_LIBS
ARG PHP_VERSION
ARG CADDY_VERSION
ARG CBROTLI_VERSION
ARG MERCURE_VERSION
ARG VULCAIN_VERSION
ARG SPC_COMMIT
# No WORKDIR: BuildKit stamps the directory one creates with
# SOURCE_DATE_EPOCH, which build.yml sets to the commit time, and that gave
# this whole stage a new cache key on every commit. Every path below is
# absolute instead.
# spc from source at the pinned commit: git checks a fetch by commit against
# its hash, and composer installs from that commit's own lock file.
# build-static.sh skips its nightly download when dist/spc is executable, so
# the wrapper there runs this checkout instead, and records each subcommand
# for test/pinned-spc.sh. Both downloads get three retries, 10s, 30s and 90s
# apart, so one dropped connection does not cost the daily slot.
RUN set -eux; \
    retry() { \
        for delay in 10 30 90; do "$@" && return 0; sleep "${delay}"; done; \
        "$@"; \
    }; \
    git init -q /opt/static-php-cli; \
    retry git -C /opt/static-php-cli fetch -q --depth 1 \
        https://github.com/crazywhalecc/static-php-cli.git "${SPC_COMMIT}"; \
    git -C /opt/static-php-cli checkout -q --detach FETCH_HEAD; \
    retry composer install --working-dir=/opt/static-php-cli \
        --no-dev --no-interaction --prefer-dist; \
    mkdir -p /go/src/app/dist; \
    printf '%s\n' '#!/bin/sh' \
        "echo \"\$1\" >>/go/src/app/dist/spc.calls" \
        "exec php /opt/static-php-cli/bin/spc \"\$@\"" >/go/src/app/dist/spc; \
    chmod 0755 /go/src/app/dist/spc
# glibc
# The plugins go through XCADDY_ARGS, which build-static.sh writes into
# craft.yml. spc applies craft.yml's extra-env over the environment, so
# SPC_CMD_VAR_FRANKENPHP_XCADDY_MODULES set here would lose to its unversioned
# default and xcaddy would fetch the newest release of every plugin.
# PHP_EXTENSION_LIBS replaces craft.yml's extra libraries, whose nghttp2/3
# and ngtcp2 serve extensions this build leaves out and are linked by
# nothing. craft.yml's with-suggests stays on, as upstream ships it, so gd
# still brings its suggested codecs, libavif among them. build-static.sh
# appends brotli, which caddy-cbrotli needs, whatever this says.
# spc asks the GitHub API which release of each library to fetch, and a call
# refused at the unauthenticated rate limit fails the build. The token comes
# as a secret, which no layer, history entry or cache key ever holds, as they
# would an ARG or ENV. Without the secret GITHUB_TOKEN is empty, and spc then
# sends no token at all.
RUN --mount=type=secret,id=github_token,env=GITHUB_TOKEN \
    PHP_EXTENSIONS="${PHP_EXTENSIONS}" \
    PHP_EXTENSION_LIBS="${PHP_EXTENSION_LIBS}" \
    PHP_VERSION="${PHP_VERSION}" \
    CADDY_VERSION="${CADDY_VERSION}" \
    XCADDY_ARGS="--with github.com/dunglas/caddy-cbrotli@${CBROTLI_VERSION} --with github.com/dunglas/mercure/caddy@${MERCURE_VERSION} --with github.com/dunglas/vulcain/caddy@${VULCAIN_VERSION}" \
    /go/src/app/build-static.sh
RUN cp "$(ls /go/src/app/dist/frankenphp-linux-*)" /go/src/app/dist/frankenphp
COPY test/pinned-versions.sh /tmp/pinned-versions.sh
RUN /tmp/pinned-versions.sh /go/src/app/dist/frankenphp "${PHP_VERSION}" \
    "${CADDY_VERSION}" "${CBROTLI_VERSION}" "${MERCURE_VERSION}" "${VULCAIN_VERSION}"
COPY test/pinned-spc.sh /tmp/pinned-spc.sh
RUN /tmp/pinned-spc.sh /go/src/app/dist /opt/static-php-cli "${SPC_COMMIT}"
COPY test/extensions.sh /tmp/extensions.sh
RUN /tmp/extensions.sh /go/src/app/dist/frankenphp "${PHP_EXTENSIONS}"

FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS fetch
ARG LIBRESPEED_VERSION
ARG LIBRESPEED_COMMIT
# git checks a fetch by commit against its hash. The tag is asked for
# separately, peeled if it is ever annotated, so a tag moved upstream fails
# the build instead of shipping a commit no release names. Network steps get
# the frankenphp stage's retries.
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

COPY --from=frankenphp-build --chmod=0755 /go/src/app/dist/frankenphp /usr/local/bin/frankenphp
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

LABEL org.opencontainers.image.source="https://github.com/divbast/librespeed-oci-image" \
    org.opencontainers.image.url="https://github.com/divbast/librespeed-oci-image" \
    org.opencontainers.image.title="LibreSpeed" \
    org.opencontainers.image.version="${LIBRESPEED_VERSION}"

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
