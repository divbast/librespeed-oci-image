<!-- SPDX-License-Identifier: BSD-2-Clause -->
<!-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee> -->

# librespeed-oci-image

A security hardened [LibreSpeed](https://github.com/librespeed/speedtest)
image: the speed test's modern UI and its PHP backend, as one standalone
server.

Published as `ghcr.io/divbast/librespeed`.

## Base

This image runs [FrankenPHP](https://github.com/php/frankenphp): PHP 8.5 and
Caddy in one binary, on a Debian Trixie-based distroless base. The binary is
the one [divbast/frankenphp](https://github.com/divbast/frankenphp) builds
for this image, `ghcr.io/divbast/frankenphp:librespeed`, with LibreSpeed's
PHP extensions and from pinned sources. The Containerfile copies it in by
digest, and CI builds nothing until it has checked that divbast/frankenphp's
own build on its `main` attested that digest.
[Verifying the image](#verifying-the-image) shows how to check the image,
and the binary in it, yourself.

## Container Features

| Property | Value |
|---|---|
| Capabilities | none (`--cap-drop ALL`) |
| Root filesystem | read-only |
| User | uid/gid 65532 (`nonroot`) |
| Port | 8080 (speed test), 8081 (health, do not publish), and nothing else listens: Caddy's admin API is off |
| tmpfs required | `/tmp`, 16 MiB (`--tmpfs /tmp:size=16m`) |
| Volume | `/data`, required only with `TELEMETRY=true` |
| Processes | one: FrankenPHP, as PID 1 |
| Shell in image | none |
| Package manager | none |
| Outbound network | none |
| Healthy | the shipped probe: `/healthz` 200 **and** `GET /backend/getIP.php` from loopback exactly `{"processedString":"127.0.0.1","rawIspInfo":""}` |

16 MiB of `/tmp` is what the acceptance suite gives every container it runs.
The speed test's uploads never land there: PHP reads no request body over
8 KiB (`post_max_size = 8K`), and an upload is 20 MiB, so PHP never reads
one, and `/tmp` does not grow however many tests run at once. What `/tmp`
holds is:

- `/tmp/librespeed`: the page, its settings and the server's
  configuration, which the bootstrap writes on every start, and the lock
  file the stats login takes;
- `/tmp/caddy`: Caddy's own state, the configuration it autosaves among it;
- the session files PHP makes for visits to the stats page.

Nothing in the image opens a connection out. PHP's URL streams are off,
there is no ipinfo.io API key, and ISP names come from the offline database
LibreSpeed ships. The stability test offers to ping Google, Cloudflare or
Apple as an external target; the visitor's browser makes those requests,
never the container.

## Running

```bash
podman run -d --name librespeed \
  --cap-drop ALL --read-only \
  --user 65532:65532 \
  --tmpfs /tmp:size=16m \
  -p 8080:8080 \
  ghcr.io/divbast/librespeed:6.3.1
```

That records nothing and needs no volume. To record results, hand out share
links and read them back on a stats page:

```bash
podman run -d --name librespeed \
  --cap-drop ALL --read-only \
  --user 65532:65532 \
  --tmpfs /tmp:size=16m \
  -v librespeed-data:/data \
  -p 8080:8080 \
  -e TELEMETRY=true \
  -e STATS_PASSWORD=... \
  -e GDPR_EMAIL=privacy@example.org \
  ghcr.io/divbast/librespeed:6.3.1
```

A named volume takes `/data`'s owner, uid 65532, from the image. A host
directory must be writable by uid 65532 inside the container. Before you
publish the stats page, read [Reverse proxy notes](#reverse-proxy-notes): how
the server sees client addresses decides whether its login limit counts
each client or all of them together.

Every setting applies on every start: change a variable and restart the
container. Only what [Telemetry and stats](#telemetry-and-stats) puts in
`/data` outlives a start.

What is served on 8080 is an allowlist:

- `/`, the modern UI, rendered from upstream's page with `TITLE`, `TAGLINE`
  and `GDPR_EMAIL` in place of its own text, and what it loads;
- `/stability.html`, the stability test;
- `/backend/empty.php`, `/backend/garbage.php` and `/backend/getIP.php`, the
  speed test's endpoints;
- `/settings.json` and `/server-list.json` (also as `/servers.json`),
  generated on every start. The server list names this server alone;
  upstream's list of public servers is never served;
- with `TELEMETRY=true`, the results routes below.

Everything else is a 404: upstream's classic UI, its examples, its
`sanitycheck.php`, its helpers and data files, and its own addresses for the
modern UI, `/index.html` and `/index-modern.html`.

The access log goes to stdout and PHP's errors to stderr, both as JSON.

`:6.3.1` names the LibreSpeed version, and `:latest` the newest image. Each
new image of a version moves that version's tag.

## Health

A speed test that serves its page can still be broken: an ISP database that
does not open, or a results database that answers every result with a 200
and records none. The image therefore ships its own probe and declares a
`HEALTHCHECK`. Docker runs it with no configuration.

Podman does not. Podman's `libimage` reads a healthcheck only out of a
Docker-media-type manifest, and this image is published as an OCI index, so
Podman's inspection path never looks at the config's healthcheck field —
[podman/podman#25454](https://github.com/containers/podman/issues/25454) and
[#18904](https://github.com/containers/podman/issues/18904) track it. The
healthcheck is in the image config; a Podman user has to pass it explicitly:

```bash
podman run -d --health-cmd '["/usr/local/bin/frankenphp","php-cli","/usr/local/bin/librespeed-health"]' ghcr.io/divbast/librespeed:6.3.1
```

Under a Quadlet, the equivalent is:

```
HealthCmd=["/usr/local/bin/frankenphp","php-cli","/usr/local/bin/librespeed-health"]
```

The array above has no leading `"CMD"` on purpose: Podman before 5.8.0
re-splits an array that starts with `"CMD"` into one useless token, so
omitting it is what makes the check work across versions.

Kubernetes ignores an image `HEALTHCHECK` entirely, on either engine, and
wants the probe declared in the pod spec instead.

| | |
|---|---|
| `/usr/local/bin/librespeed-health` | The in-container probe `HEALTHCHECK` runs. Asserts `/healthz` is 200 **and** `GET /backend/getIP.php` on 8080 answers exactly `{"processedString":"127.0.0.1","rawIspInfo":""}` |
| `http://127.0.0.1:8081/healthz` | 200 `ok`, or 503 with one line, `<check>: <reason>`, for each check that failed |

`/healthz` runs every check on every request:

- `ini`: inside the server, `allow_url_fopen` and `display_errors` are off
  and `log_errors` is on;
- `phar`: upstream's `geoip2.phar` loads, and holds the reader for the ISP
  database;
- `mmdb`: the ISP database opens, and answers `1.1.1.1` with `AS13335`, a
  name and a country, the fields `getIP.php` reads;
- `database`: with `TELEMETRY=true`, `/data/telemetry.db` is there, it and
  `/data` are writable, and it holds upstream's table. It is opened
  read-only, so the check never makes a database it finds missing.

`/healthz` never goes through 8080's routes, so only `getIP.php`'s exact
answer shows Caddy routing the speed test to PHP and PHP running upstream's
code. The probe asks both. Port 8081 serves nothing else and is meant to
stay unpublished.

There is no shell in this image, so a health command given as a plain string —
which Docker and Podman both run through `/bin/sh -c` — can never pass. An
explicit command needs the JSON array form, and the form is not the same
everywhere. Compose's `healthcheck.test` wants a leading `CMD`:

    test: ["CMD", "/usr/local/bin/frankenphp", "php-cli", "/usr/local/bin/librespeed-health"]

Podman's `--health-cmd` and a Quadlet's `HealthCmd=` do not — see above.

Kubernetes does not either. An `exec` probe takes the argv and nothing else; a leading
`"CMD"` fails with `"CMD": executable file not found`:

    exec:
      command: ["/usr/local/bin/frankenphp", "php-cli", "/usr/local/bin/librespeed-health"]

Prefer that to an `httpGet` probe against `/healthz`. `httpGet` reaches a
`containerPort` directly and so needs no publishing, but it cannot check
`getIP.php`'s exact answer, and `/healthz` on its own is only half of what
healthy means here.

Two answers that look like defects and are not:

- **A full `/data` passes every check.** `is_writable()` tests permission, not
  free space
- **`http://host:8080/healthz` is a 404.** The health endpoint is only ever
  on 8081

There is no shell and no access log on `:8081`, so the reason is read back
off the engine. The probe passes the 503's lines on:

    docker inspect --format '{{json .State.Health}}' <container>

which holds lines such as
`librespeed-health: http://127.0.0.1:8081/healthz: expected 200, got 503: database: missing`.

PHP's own errors never reach a response. `display_errors` is off, and real
errors (fatal, parse, compile and core errors, and uncaught exceptions) go to
the container's stderr as JSON. Warnings, notices and deprecations are not
logged at all, so no client can fill the log with them. `/healthz` fails
naming `ini` if those settings are not in force.

**Do not set `PHP_INI_SCAN_DIR` in the container's environment.** The server
reads it too, and it overrides the image's PHP settings in
`/etc/frankenphp/php.d`: a value without a leading `:` replaces that
directory, and one with a leading `:` adds a directory whose settings win
over the image's. If that turns `allow_url_fopen` or `display_errors` on, or
`log_errors` off, `/healthz` reports `ini` with a 503 and the container goes
unhealthy. It checks only those three: a `post_max_size` raised that way
would have PHP spool every upload into `/tmp` again, and nothing would say
so.

## Env Vars

| Variable | Default | Notes |
|---|---|---|
| `TITLE` | upstream's | The page's heading, and its title as "TITLE - Free and Open Source Speedtest" |
| `TAGLINE` | upstream's | The line under the heading |
| `GDPR_EMAIL` | upstream's placeholder | The address the privacy policy gives for deletion requests. Set it with `TELEMETRY=true`, which is when the page shows that policy |
| `TELEMETRY` | `false` | `true` records every result in SQLite on `/data`, which must then be a volume uid 65532 can write, and serves the results routes |
| `STATS_PASSWORD` | *none* | With `TELEMETRY=true`, serves `/results/stats.php` behind the image's own login, with this password. At most 72 bytes |
| `REDACT_IP_ADDRESSES` | `false` | With `TELEMETRY=true`, records `0.0.0.0` in place of the client's address, in the result and in its ISP information |
| `ENABLE_ID_OBFUSCATION` | `false` | With `TELEMETRY=true`, hands out result ids scrambled with a salt kept in `/data`, so one share link does not give away the next |
| `TRUSTED_PROXIES` | *none* | Addresses and CIDR ranges, separated by commas or spaces, of the reverse proxies whose `X-Forwarded-For` names the client, for the stats login's limit and the access log alone. See [Reverse proxy notes](#reverse-proxy-notes) |

- An empty value counts as unset.
- The switches take exactly `true` or `false`. Anything else stops the
  start and the log names the variable, so a typo never quietly means the
  default.
- `TITLE`, `TAGLINE` and `GDPR_EMAIL` are HTML-escaped into the page, and
  one that is not valid UTF-8 stops the start, naming the variable. Docker's
  `-e` never gets that far: it sends the environment as JSON, which puts
  U+FFFD in place of such bytes, so they show as replacement characters. No
  value is ever written into PHP as code.
- With telemetry off, `STATS_PASSWORD`, `REDACT_IP_ADDRESSES` and
  `ENABLE_ID_OBFUSCATION` do nothing, and the log names those that are set.
  Their values are still checked.
- `STATS_PASSWORD` never reaches the server's environment: the server has
  only its hash.
- `TRUSTED_PROXIES` takes addresses and ranges and nothing else. Caddy's own
  shorthands, such as `private_ranges`, stop the start, and so does a range
  of every address, such as `0.0.0.0/0` or `::/0`: every client would then
  count as a proxy, and could choose the address its failed logins are
  counted against.

Upstream's own image reads more variables than these. This one reads none
of the others: `PASSWORD` (here `STATS_PASSWORD`), `MODE`, `WEBPORT`,
`DB_TYPE`, `OBFUSCATION_SALT` and `IPINFO_APIKEY` among them do nothing here.

## Telemetry and stats

With `TELEMETRY=true` the page records each finished test through
`/results/telemetry.php`, at upstream's `basic` level: the results, the ISP
line `getIP.php` gave, the client's address, user agent and browser
language, and no test log. `/results/json.php?id=…` shows a result as JSON,
and `/results/?id=…`, which the page links to, draws it as a PNG to share.
`/results/stats.php`, which shows the latest results, addresses included,
and finds one by id, is served only when `STATS_PASSWORD` is set.

The results live in `/data/telemetry.db`, which upstream's own code makes
at the first start, so its schema is always upstream's. The container
refuses to start, and logs why, if `/data` is not writable by uid 65532, if
an existing `telemetry.db` is not readable and writable by it, if SQLite
cannot open it, or, with `STATS_PASSWORD` set, if an existing
`stats-login.db` is not readable and writable by it. Served, any of the
first three would answer every result with an empty 200 and record none,
and the last would answer every login with a 500. After restoring a backup
as root, give the files back to uid 65532.

**The salt.** With `ENABLE_ID_OBFUSCATION=true`, the first start writes
`/data/idObfuscation_salt.php`, readable by uid 65532 alone, and no later
start changes it. Every share link handed out was made with it, and another
salt would break them all. So it and `telemetry.db` are one restore unit: a
database restored without its salt gets a new one at the next start, and
every old link stops working. A salt uid 65532 cannot read stops the start,
since upstream would read it as 0 and change every id. Turning obfuscation
off leaves the salt where it is.

**The stats login.** Upstream's `stats.php` checks its password itself, as
plain text. The image takes that away. Caddy sends upstream's login form,
and every POST to `stats.php`, to the image's own login, and upstream's own
password is a new random value on every start, which nothing serves or
logs. The login:

- checks the password against a bcrypt hash of `STATS_PASSWORD`, written to
  `/data/stats-password.hash`, readable by uid 65532 alone, on every start.
  bcrypt reads only the first 72 bytes, so a longer `STATS_PASSWORD` stops
  the start, and a longer password submitted is wrong;
- on success starts a new session, so a session id someone planted
  beforehand is never the one logged in, with an `HttpOnly`,
  `SameSite=Lax` cookie;
- allows a client 5 failures in 15 minutes. After that the client gets a
  429, with `Retry-After`, until the oldest of the five is 15 minutes old,
  even with the right password. IPv4 counts per address, IPv6 per /64,
  which is what one subscriber is usually given, and an IPv4-mapped IPv6
  address as the IPv4 address it is. The failures are kept in
  `/data/stats-login.db`, so a restart does not clear them;
- checks one password at a time, server-wide. An attempt that arrives while
  another is being checked gets an immediate 429, and is not counted;
- answers a client already at its limit with its 429 before it touches that
  one-at-a-time check, so a client that is locked out cannot keep the check
  busy for everyone else.

The client is the address the connection comes from, or, from a proxy
`TRUSTED_PROXIES` names, the address its `X-Forwarded-For` gives.

## Reverse proxy notes

**`TRUSTED_PROXIES`.** Behind a reverse proxy, every connection comes from
the proxy, so without `TRUSTED_PROXIES` every visitor shares one count of
failed logins, and anyone's five wrong guesses keep the admin out. Name the
proxy, and for connections from it Caddy takes the client from
`X-Forwarded-For`, reading right to left and skipping every address
`TRUSTED_PROXIES` names. Name only proxies that append the address they saw
to the `X-Forwarded-For` they pass on. One that passes on a client's own
header unchanged lets the client write the right-most address, and so pick
its own bucket. The layout the acceptance suite tests is the proxy and the
container on one network, 8080 unpublished, and `TRUSTED_PROXIES` naming
the proxy's address, or a subnet that holds nothing but proxies.

**Engines that rewrite client addresses.** From another machine, ask the
published port directly:

    curl http://<host>:8080/backend/getIP.php

`processedString` is the address the server sees. If it is not that
machine's own, the engine rewrites source addresses. Rootless Podman does
this for a container on a network made with `podman network create`: every
connection to a published port arrives from the container's own address.
Then every client lands in one bucket, so anyone can keep the admin locked
out, and recreating the container, which draws a new address, clears a
lockout. The address shown and recorded for every visitor is that one too.
Use a network mode or a proxy that preserves client addresses, or put a
proxy that appends `X-Forwarded-For` in front, on the container's network,
and set `TRUSTED_PROXIES` to it.

**Uploads.** The speed test uploads in requests of 20 MiB, 4 MiB from Chrome
on mobile, so a proxy must pass request bodies of at least 20 MiB. One that
refuses them answers 413, and the test reports an upload speed that is
wildly wrong. Clients that send `Expect: 100-continue`, as curl-based tools
do for large bodies, upload 0 bytes to `empty.php` and still get a 200,
because the server answers without ever asking for the body, so a
curl-based measurement of upload speed is wrong too. Browsers send no such
header and are unaffected; with curl, `-H 'Expect:'` turns it off.

**Shown and recorded addresses can be spoofed.** Upstream's `getIP.php`,
which shows each visitor their address and ISP, and `telemetry.php`, which
records the address with each result, take it from `Client-IP`,
`X-Real-IP`, `X-Forwarded-For` or `CF-Connecting-IPv6` when a request
carries one, whoever sent it. So any visitor can make the address shown and
recorded whatever they like, and its ISP line with it. `TRUSTED_PROXIES`
changes nothing here. That is upstream's behaviour, and this image leaves
it alone.

**Serve it at the root of a host name.** The server list points the test at
`/backend`, so under a path prefix the test would go to whatever answers
`/backend` at the root.

## Dependency updates

Every input the Containerfile names is pinned: the fetch stage's Debian
image and the distroless runtime by digest, the FrankenPHP binary by the
digest of `ghcr.io/divbast/frankenphp:librespeed`, and LibreSpeed by version
and the commit its tag names. Two packages the fetch stage installs are not:
`git` and `ca-certificates`, with what they depend on, come from Debian's
archive as it stands on the day of the build. They only fetch LibreSpeed
and check its tag; the image takes nothing from that stage but LibreSpeed's
own files.

The binary's own inputs, PHP, Caddy, its plugins and static-php-cli, are
pinned in divbast/frankenphp, which also fails its build unless the binary
loads exactly the PHP extensions this image needs. The C libraries
static-php-cli builds into it are not pinned there: openssl, zlib and
several others come from their newest release, and libaom from its main
branch, whenever the binary is built. So a new `:librespeed` digest can
carry newer C code with no pin moved, and that is how fixes to those
libraries arrive.

A pin only moves through a pull request, and
[Renovate](https://docs.renovatebot.com/) opens those. It runs every morning,
after every merge to `main`, whenever CI finishes on one of its branches and
when the Dependency Dashboard issue is edited, from
`.github/workflows/renovate.yml`, which calls divbast/ci's shared Renovate
workflow, as the divbast org's `divbast-renovate` GitHub App. Its settings
are divbast/renovate-config's shared presets, and `renovate.json` adds only
what LibreSpeed's releases need:

- A LibreSpeed release arrives as one pull request that moves the version
  and the commit together, and the build fails if the tag no longer names
  that commit.
- A new `:librespeed` binary arrives as a pull request that moves the
  digest, and CI verifies the new digest's attestation before it builds.
- A release of divbast/ci arrives as one pull request that moves every
  workflow's pin of it together.

Renovate merges its own pull requests once every check on one has passed,
LibreSpeed releases included, while the repository variable
`LIBRESPEED_AUTOMERGE` is `enabled`. Two kinds first wait for someone to
approve them on the Dependency Dashboard: a new major version of divbast/ci,
and a raise of `go.mod`'s `go` line. Any other value of the variable, or
none, stops merging from the next run, and Renovate goes on opening and
rebasing pull requests for a person to merge:

    gh variable set LIBRESPEED_AUTOMERGE --body disabled

After a merge it rebases every open update the merge left behind, so each is
tested again against the new `main` before it merges. It merges with a
squash, which GitHub signs, because `main` accepts only signed commits.

**At most one new image a day.** A merge does not publish. The daily `build`
run publishes `main` if anything merged since the last image, so however many
updates land in a day, users see one new image at most. An urgent fix need not
wait: a manual run of the `build` workflow from `main` (Actions, build, Run
workflow) publishes at once. Neither ever replaces `:latest` with an image of
an older commit.

A daily stall check, `.github/workflows/stall.yml`, fails, and GitHub emails
you, when a Renovate pull request has been open more than 14 days or red or
pending for more than a day, when Renovate has not succeeded in a day, or when
two daily runs should have published and did not.

The App, the `renovate` environment holding its key, `LIBRESPEED_AUTOMERGE`
and this repository's rulesets are all managed from
[divbast/infra](https://github.com/divbast/infra). A fork needs its own App:

1. Create a GitHub App with its webhook turned off and these repository
   permissions: Checks, Commit statuses, Contents, Issues, Pull requests and
   Workflows read and write; Dependabot alerts and Metadata read. Install it
   on this repository only.
2. Create an environment called `renovate` whose deployment branches are
   limited to `main`. Give it a variable `RENOVATE_APP_CLIENT_ID` holding the
   App's client ID, and a secret `RENOVATE_APP_PRIVATE_KEY` holding a private
   key generated for the App.
3. Run the `renovate` workflow by hand once. The Dependency Dashboard issue
   appears, listing every dependency Renovate found.
4. When you are ready for Renovate to merge on its own, run
   `gh variable set LIBRESPEED_AUTOMERGE --body enabled`.

## Verifying the image

Every image published from `main` carries an attestation of where it was
built, signed with Sigstore. With [gh](https://cli.github.com/) 2.102.0 or
newer:

    gh attestation verify oci://ghcr.io/divbast/librespeed:latest --repo divbast/librespeed-oci-image --signer-workflow divbast/ci/.github/workflows/app-publish.yml --source-ref refs/heads/main

`--repo` and `--source-ref` say the image was built from this repository's
`main` branch, and `--signer-workflow` that divbast/ci's publish workflow,
which this repository's `build` workflow calls, signed it. Older versions of
gh compare `--signer-workflow` with only the start of the signer's identity,
and `--source-ref` without regard to case.

If gh reports that its token was denied, for an image anyone may pull, it
is sending a registry login from Docker's configuration, such as a stale one
or what a credential helper hands it, which GHCR refuses. Point
`DOCKER_CONFIG` at an empty directory:

    mkdir -p /tmp/empty-docker-config
    DOCKER_CONFIG=/tmp/empty-docker-config gh attestation verify oci://ghcr.io/divbast/librespeed:latest --repo divbast/librespeed-oci-image --signer-workflow divbast/ci/.github/workflows/app-publish.yml --source-ref refs/heads/main

Images published before this repository moved onto divbast/ci's shared
workflows were signed by its own `build` workflow instead, so the command
fails for them. Verify one of those with
`--signer-workflow divbast/librespeed-oci-image/.github/workflows/build.yml`.
Every image published since the move is signed by divbast/ci's
`app-publish.yml`.

**The FrankenPHP binary.** The image's binary is copied from
`ghcr.io/divbast/frankenphp:librespeed`, by the digest the Containerfile's
`COPY --from=` names. Take that digest from the Containerfile at the commit
the image was built from, its `org.opencontainers.image.revision`:

    grep -o 'frankenphp:librespeed@sha256:[0-9a-f]*' Containerfile

and, with `<digest>` standing for its `sha256:…` part, check that
divbast/frankenphp's own build on its `main` branch made it:

    gh attestation verify oci://ghcr.io/divbast/frankenphp@<digest> --repo divbast/frankenphp --signer-workflow divbast/frankenphp/.github/workflows/build.yml@refs/heads/main --source-ref refs/heads/main

CI runs the same check before it builds anything, so a binary that fails it
never reaches an image built here.

## Licences in the image

The image's `org.opencontainers.image.licenses` label is
`BSD-2-Clause AND LGPL-3.0-or-later AND CC-BY-SA-4.0 AND Apache-2.0 AND OFL-1.1`:

| Licence | What |
|---|---|
| BSD-2-Clause | This repository: the Containerfile, the bootstrap, the health probe and endpoint, the stats login and the Caddy and PHP configuration |
| LGPL-3.0-or-later | LibreSpeed itself. Its files ship unchanged, apart from the page and settings the bootstrap renders from upstream's own on every start, and its `LICENSE` ships at `/var/www/librespeed/LICENSE` |
| CC-BY-SA-4.0 | `backend/country_asn.mmdb`, the ISP database (below) |
| Apache-2.0 | `results/OpenSans-Light.ttf` and `results/OpenSans-Semibold.ttf`, Open Sans 1.10, which the share image is drawn in |
| OFL-1.1 | `frontend/fonts/Inter-latin.woff2` and `frontend/fonts/Inter-latin-ext.woff2`, Inter 4.001 by The Inter Project Authors, which the page is set in |

IP address data powered by [IPinfo](https://ipinfo.io). The bundled
`backend/country_asn.mmdb` is IPinfo's free IP to Country ASN database,
licensed under [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/),
and ships unchanged, as the LibreSpeed release carries it. The image never
fetches a newer copy, since it makes no outbound connection, so its ISP
names are as current as the copy that release carries.

The label covers this repository, LibreSpeed, and the data and fonts above.
The FrankenPHP binary divbast/frankenphp builds (PHP, Caddy and the
libraries built into it), the distroless base image, and the MaxMind reader
and Composer autoloader inside upstream's `geoip2.phar` are other projects'
work, and carry their own licences.

## AI/LLM Disclosure

This project was developed with significant LLM involvement. Each git commit contains an `Assisted-by:` tag detailing the particular model/tool used.

## Licence

Copyright 2026 Aryan Ameri.

[BSD-2-Clause](LICENSES/BSD-2-Clause.txt)

This project is [REUSE](https://reuse.software/) compliant.
