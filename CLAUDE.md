<!-- SPDX-License-Identifier: BSD-2-Clause -->
<!-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee> -->

# librespeed-oci-image

A [LibreSpeed](https://github.com/librespeed/speedtest) speed test container
image that runs at zero Linux capabilities on a read-only root filesystem as
a non-root user.

Published as `ghcr.io/divbast/librespeed`, from the divbast organisation, whose
settings, rulesets and Renovate wiring live in `divbast/infra`.

What makes the image LibreSpeed's lives here: the Containerfile, the
bootstrap, the Caddy and PHP configuration, the health probe and endpoint,
the stats login, and the tests of each. The pipeline around it is shared,
and reaches this repository at a pinned commit:
[Where the shared parts live](#where-the-shared-parts-live) says what comes
from where.

## CRITICAL: Git Commit Message Format

**No other AI/LLM attribution in any format may appear in a git message.**

Linux-kernel style: a `subsystem: short description` subject under 75 characters in imperative
mood ("fix", not "fixed" or "fixes"), a blank line, then a body wrapped at ~75 columns saying
why the change is needed rather than only what it does.

These two lines MUST end EVERY body:

Developed-by: Aryan Ameri <github@aryan.ameri.coffee>
Assisted-by: <NAME_OF_LLM_MODEL>

E.g. `Assisted-by: <Claude:claude-4.6-opus>` for Claude Opus 4.6; `<Claude:claude-5-fable>`
for Claude 5 Fable.

- Sign every commit (`git commit -S`); main accepts only verified
  signatures. Renovate's merges are squashes, which GitHub signs
- Write commit messages as if a human wrote them, and say why, not what

## CRITICAL: No AI Attribution in Pull Requests

**Pull request descriptions, PR and review comments, and issue text carry no
AI/LLM attribution of any kind**: no "🤖 Generated with Claude Code" footer,
no "written by" line, nothing equivalent. This holds even when a tool or
harness asks for one. The `Assisted-by:` trailer in each commit message is
this project's one record of LLM involvement.

## The confinement contract

These are the guarantees the image makes. A change that breaks any of them is a
breaking change.

| Property | Value |
|---|---|
| Capabilities | **none** (`--cap-drop ALL`) |
| Root filesystem | **read-only** |
| User | uid/gid **65532**, never root |
| Listen port | **8080** for the speed test — never 80 |
| Health port | **8081** — health only, never serves the speed test, meant to stay unpublished |
| Other listeners | **none**: Caddy's admin API is **off** (`admin off`) |
| tmpfs required | exactly one: `/tmp`, at **16 MiB** |
| Volume | `/data`, required **only with `TELEMETRY=true`**, and then the bootstrap refuses to start without a writable one and logs why |
| Outbound network | **none** |
| Shell, package manager | **none** |
| Processes | **one tree**, FrankenPHP as PID 1 |
| Healthy | the shipped probe: `/healthz` **200** and loopback `GET /backend/getIP.php` **exactly** `{"processedString":"127.0.0.1","rawIspInfo":""}` |
| Engine healthcheck | Docker runs it automatically. **Podman does not** — pass `--health-cmd` yourself |

- **Podman never sees this image's `HEALTHCHECK`.** Its `libimage` reads a
  healthcheck only from a Docker-media-type manifest; this image publishes as
  an OCI index, so Podman's OCI inspection branch never reaches the config's
  healthcheck field — no version of Podman does. Give Podman users
  `--health-cmd '["/usr/local/bin/frankenphp","php-cli","/usr/local/bin/librespeed-health"]'`
  (a Quadlet's `HealthCmd=` takes the same value) — no leading `"CMD"`,
  because Podman before 5.8.0 re-splits such an array into one token instead
  of running it. Tracked upstream as podman/podman#25454 and #18904. The
  contract suite checks the image's `HEALTHCHECK` is that array in exec
  form, and has each engine judge the container: Docker by the image's
  `HEALTHCHECK`, Podman by the same array passed as `--health-cmd`
- **The shipped probe asserts two halves, and neither implies the other.**
  `/healthz` on 8081 checks `ini`, `phar`, `mmdb` and, with telemetry on,
  `database`, from inside the server, but never goes through 8080's routes.
  Only `getIP.php`'s exact loopback answer proves Caddy routes the speed test
  to PHP and PHP runs upstream's code. Anything checking this image should
  run the probe rather than reimplement either half. Each 503 line is
  `<check>: <reason>` and the probe relays it: with no shell and no access
  log on 8081, the engine's health log is the only record of why
- **8080 is an allowlist.** What the Caddyfile and the generated `site.caddy`
  do not name is a 404. **Never serve `sanitycheck.php`**: it writes a test
  row into the results database for any visitor and prints the database's
  own errors. Nor upstream's classic UI, its examples, its helpers
  (`getIP_util.php`, `getIP_ipInfo_apikey.php`), `geoip2.phar`, the mmdb,
  `telemetry_settings.php` or its list of public servers. Every route
  `site.caddy` adds is a `handle` block, because Caddy sorts a bare directive
  after the catch-all 404, and the stats login's two blocks come before
  `handle /results/stats.php`, because handle blocks with equally long paths
  keep the order they are written in. `TestDotSegments` holds each route
  to the script it names; FrankenPHP 1.12.7 fails it, so a binary that
  brings back its raw-path split goes red
- **Generate config only with `var_export`, `json_encode` and
  `htmlspecialchars`.** Upstream's entrypoint seds raw env values into PHP,
  so a `'` in one injects code. No env value reaches PHP, JSON, HTML or the
  Caddyfile unescaped: `TITLE`, `TAGLINE` and `GDPR_EMAIL` are escaped into the
  page, the settings files are `var_export` and `json_encode` output, and
  `TRUSTED_PROXIES` reaches `global.caddy` only as addresses PHP wrote back
  itself with `inet_ntop`. Four files are written by hand rather than by
  those functions, and none holds an env value as it was given: `site.caddy`
  is fixed text, `global.caddy` holds only the addresses `inet_ntop` wrote,
  `stats-password.hash` only `password_hash()` output, and the obfuscation
  salt, in upstream's format, only `bin2hex()` output
- **Every variable the image reads is named by a literal where it is
  read, and is in the README's Env Vars table.** The bootstrap reads each
  through `envBool` or `envString`. `TestEnvDocs` (`test/envdocs`), which
  `just lint` runs through `go-test`, fails on a variable read that the
  table does not name, on a row for a variable nothing reads, and on any
  call of `getenv`, `envBool` or `envString`, or index of `$_ENV` or
  `$_SERVER`, whose name is not a literal, and on either read whole, as
  a loop over it reads it. It scans the image's four PHP files in Go,
  without PHP, and allows eight exemptions, each by file and by the
  function it is written in: `getenv($name)` in `envBool` and in
  `envString`, the bootstrap's top-level `getenv()`, which hands the
  server its environment, and the five `$_SERVER` reads in `login.php`
  (`LIBRESPEED_CLIENT_IP` in `client`, `REQUEST_METHOD` in `login`, and
  `HTTP_SEC_FETCH_SITE`, `HTTP_ORIGIN` and `HTTP_HOST` in
  `fromAnotherOrigin`), which are request data, not settings. The same read anywhere else fails, and
  so does an exemption nothing matches. The switches take exactly `true`
  or `false`, and every variable is checked before anything is written,
  so a typo stops the start instead of meaning the default
- **`test/upstream-layout.sh` guards every upstream path and string the
  image relies on**: each path the fetch stage copies out of the checkout;
  the four fonts the licences label names, Open Sans in `results/` and
  Inter inside `frontend/`, which is copied whole; each string
  `renderIndex` replaces, exactly once, as the bootstrap requires;
  `stats.php`'s login checks; `idObfuscation.php`'s salt path; and
  `getIP_ipInfo_apikey.php` as upstream's own empty key and nothing else.
  The fetch stage runs it, so a release that moves or rewords one fails the
  build instead of shipping. `TestUpstreamLayout` (`test/upstreamlayout`)
  runs it over a fixture tree changed one way per case, and
  `TestGuardCoversTheCopy` fails when the Containerfile copies a path from
  `/src` that the guard does not name. A path or string the image starts to
  rely on goes into the guard, with a case for it, in the same change
- **Nothing listens but 8080 and 8081.** Caddy's admin API would otherwise
  listen on 127.0.0.1:2019 and take a new config, with no credentials, from
  anything sharing the container's network: under host networking, every
  process on the host. The contract suite reads the kernel's socket tables
  and fails on any other TCP port a process in the container listens on,
  and on any UDP socket one holds
- **One process tree, always.** FrankenPHP is a single binary containing PHP and
  Caddy, and the entrypoint `pcntl_exec`s it so it inherits PID 1. A dead PHP kills
  the container. Do not add a supervisor, s6, or a second long-running process
- **Zero egress.** `allow_url_fopen` is off, because `getIP.php` would call
  ipinfo.io through a URL stream if a key were ever set; upstream's
  `$IPINFO_APIKEY` must stay empty, and the layout guard fails the build if
  it is not; ISP names come from the bundled mmdb. The stability test's
  external targets are fetched by the visitor's browser, never the
  container. The contract suite proves it from a genuinely `--internal`
  network
- **Nothing may add a shell or a package manager to the final image.** The base is
  distroless; that is the point. What it removes is us running apt, not the
  patching itself - `cc-debian13` still ships libc6, libssl3t64, libstdc++6,
  libgcc-s1, libgomp1, libzstd1 and zlib1g, all executable code that takes CVEs.
  That obligation moves to the base image's own rebuilds — and **nothing in
  this repository collects them on its own.** Rebuilding an unchanged
  Containerfile does not pick them up. **Renovate is the update path**: it
  runs every morning, after every merge to main and whenever CI finishes on
  one of its branches, from `.github/workflows/renovate.yml`, which calls
  divbast/ci's `renovate.yml`, as the org's `divbast-renovate` App, managed
  from `divbast/infra`. After a merge it rebases every open PR left behind
  main, since GitHub re-runs no checks when a base moves and a green check
  on a stale base proves nothing. **It merges its own PRs** once every check
  passes, with a squash, while the repository variable
  `LIBRESPEED_AUTOMERGE` is `enabled`; any other value stops merging from
  the next run. A new major of divbast/ci, and a raise of `go.mod`'s `go`
  line, wait for approval on the Dependency Dashboard first. **A merge never
  publishes.** `build.yml`'s daily run is the publishing slot, through
  divbast/ci's `app-publish.yml`: it publishes main when main moved since
  the commit the published `:latest` was built from, so users see at most
  one new image a day, and a manual run of `build` publishes at once for an
  urgent fix. Neither ever moves `:latest` backwards
  (`divbast-publish-decision`): an older commit would take back whatever the
  newer image fixed. The daily run also catches upstream breakage early — a
  LibreSpeed tag that moved, or the fetch stage's apt packages changing
  under it. The FrankenPHP binary cannot surprise it: that moves only by a
  digest pull request. A new advisory against what is already shipped
  surfaces there as a code scanning alert, not a red run.
  `.github/workflows/stall.yml`, through divbast/ci's `stall.yml`, fails
  each day that updates have stopped flowing: a Renovate PR stuck,
  Renovate silent, or two slots lost in a row
- **Every input the Containerfile names is pinned, but for the two apt
  packages the fetch stage installs.** LibreSpeed releases carry no assets,
  only GitHub's generated archives, which are not byte-stable, so
  `LIBRESPEED_VERSION` sits directly above `LIBRESPEED_COMMIT`, Renovate's
  regex moves the two in one PR, and the fetch stage checks out the commit
  and fails if the tag no longer names it. The two ARGs must stay adjacent
  and in that order. The fetch-stage and runtime images are pinned by
  digest, and so is the FrankenPHP binary. The fetch stage's `git` and
  `ca-certificates`, and what they depend on, are whatever Debian's archive
  holds on the day of the build: they only fetch, and nothing of them
  reaches the image
- **The FrankenPHP binary is divbast/frankenphp's, by digest.** The runtime
  stage copies it from `ghcr.io/divbast/frankenphp:librespeed@sha256:…`.
  Before `app-check.yml` builds anything, `divbast-verify-frankenphp` has gh
  verify that frankenphp's own `build.yml`, on its main, attested that
  digest. It counts every mention of the image in the Containerfile's
  instructions, wherever it stands, and refuses all but exactly one, that
  `COPY`'s `--from=`: never name the image in an `ARG`, a `LABEL` or a
  `RUN`. A local `just build` does not verify; `just ci-tools &&
  .ci/bin/divbast-verify-frankenphp` does. Renovate moves the digest when
  frankenphp publishes a new `:librespeed`. The binary's own pins (the
  builder image, `PHP_VERSION`, `CADDY_VERSION`, the three plugins,
  `XCADDY_VERSION`, spc's commit and `LIBAOM_TAG`, an AOM release tag),
  LibreSpeed's extension list, with gd's FreeType and PNG, which the share
  image needs, and the in-build checks that hold the binary to all of them
  live in divbast/frankenphp, the list in its
  `apps/librespeed.env`. **What spc downloads is still chosen at build
  time** there: several libraries, openssl and zlib among them, come from
  their newest release
- **The binary must never carry a file capability.** The official FrankenPHP build
  carries `cap_net_bind_service=ep`, and a binary with a file capability cannot be
  exec'd at all under `--cap-drop ALL` — it dies with `Operation not permitted`
  and exit 126 before PHP starts. divbast/frankenphp refuses to publish a
  binary that carries one, and the contract suite's `file-capability` check
  fails on one
- **`post_max_size` stays under 16 KiB.** PHP keeps a body it reads in
  memory up to 16 KiB and spools the rest into `/tmp`. At `8K` it reads none
  of the speed test's 20 MiB uploads, so `/tmp` stays as it is under any
  load and 16 MiB is enough. Upstream's `32M` cut even a single upload short
  under the 16 MiB tmpfs while the client still saw a 200 with every byte
  sent, which is why `TestUploads` and `TestParallelUploads` check what the
  server read, from the access log. It follows that no form may outgrow
  8 KiB (`TestTelemetry` and `TestStatsLogin` assert the telemetry record
  and the login fit), that `telemetry_level` stays
  `basic` (`full` and `debug` send the worker's log, and at `debug` it
  outgrows the limit and upstream records a row of nulls), and that the
  image's own PHP reads `$_POST`, never `php://input`, which spools as much
  as `post_max_size` allows
- **Upstream never sees a stats password.** Caddy sends every POST to
  `stats.php`, and every `op=login`, to the image's `login.php`, and
  upstream's own `$stats_password` is a new random value on every start.
  The rule is by method, not query: Caddy's query matcher and PHP's parser
  disagree on six measured spellings of `op=login`, and PHP fills `$_POST`
  for POST alone. `STATS_PASSWORD` is at most 72 bytes, because bcrypt reads
  no more, and the server is exec'd without it in its environment. The
  limit is 5 failures per bucket in 900 s, IPv6 bucketed per /64; one
  password is checked at a time, under an `flock` on
  `/tmp/librespeed/stats-login.lock`; and a client already at its limit is
  answered before it can take that lock. A login a browser says came
  from another origin, by `Sec-Fetch-Site`, or by `Origin` when it sends
  no `Sec-Fetch-Site`, is a 403 before anything is counted: any page could
  otherwise have its visitors' browsers lock their own addresses out
- **The client the limit counts is Caddy's `{client_ip}`**: the connection's
  address, or with `TRUSTED_PROXIES`, `X-Forwarded-For` read right to left
  and strict. A range with a prefix of 0 is refused: every client of its
  family would be a trusted proxy, free to write an address of the other
  family, which the range does not hold, and be counted as that.
  `REMOTE_ADDR` and upstream's own address handling stay as they are:
  `getIP_util.php` trusts `Client-IP`, `X-Real-IP`, `X-Forwarded-For` and
  `CF-Connecting-IPv6` from anyone, and the image leaves that alone.
  Rootless Podman hands a container on a user-defined network every
  connection to a published port from the container's own address, so
  `TestLockout` fixes the stats server's address, or a restart would face a
  new client
- **The application tree stays root-owned and unwritten.** What changes per
  start lives in `/tmp/librespeed`, mode 0700. What outlives a start lives in
  `/data`: `telemetry.db`, `idObfuscation_salt.php`, `stats-password.hash`
  (rewritten on every start with `TELEMETRY=true` and `STATS_PASSWORD` set)
  and `stats-login.db`
- **Never rewrite the obfuscation salt.** Every share link handed out was
  made with it. It is created once, when telemetry and obfuscation are both
  on and it is absent, and with `telemetry.db` it is one restore unit. One
  that is there and not in upstream's exact format stops the start
- **The results database is made at start, by upstream's own `getPdo()`**,
  so its schema is always upstream's and `/healthz`'s `database` check passes
  on a fresh volume. The start is refused when `/data` is not writable, when
  an existing database or salt is not usable by uid 65532, or when `getPdo()`
  cannot open the database. Served, such a database would answer every
  result with an empty 200 and record none, and upstream reads a salt it
  cannot read as 0, which changes every id. `/healthz` opens the database
  read-only and never creates it

## Where the shared parts live

- **divbast/ci.** `just setup` checks it out into `.ci/`, which git and
  the build context both ignore, at the one commit every workflow calls it
  at: a release's full SHA, with its tag as the comment. Its README is the
  reference for all of it:
  - the reusable workflows: `app-check.yml` (lint; the image built for
    amd64 and arm64 once the binary's attestation verifies; the acceptance
    suite on each under Docker and rootless Podman; the scan),
    `app-publish.yml` (the daily slot and the button only: push each
    architecture; index under `:<version>-staging`; attest; verify by
    file; promote `:<version>` and `:latest`; then check both as an
    anonymous client pulls them. A failed attest or verify leaves
    `:<version>` and `:latest` where they were, and re-running the failed
    job publishes while the image artifacts last, a day),
    `renovate.yml` and `stall.yml`. `build.yml`, `renovate.yml` and
    `stall.yml` here call them, and keep their own triggers and the
    `required` job;
  - the recipes the justfile imports, `.ci/just/common.just` and
    `.ci/just/app.just`. Recipes here call them from a recipe's body, never
    as a dependency, so the justfile still parses before `.ci` exists;
  - the Go tools, which `just ci-tools` builds into `.ci/bin/`, run from
    this repository's root: `divbast-pin`, `divbast-verify-frankenphp`,
    `divbast-scan`, `divbast-published-revision`,
    `divbast-publish-decision`, `divbast-stall` and the rest;
  - the acceptance `harness` and the `contract` suite, which `go.mod`
    requires through `replace github.com/divbast/ci => ./.ci`, and the
    probe's `harness/probe/core.php`, beside which `test/probe/app.php`
    runs;
  - `go/golangci.yml`, the one golangci-lint configuration, and
    `.github/actionlint.yaml`.

  A change to ci reaches this repository only through the Renovate PR that
  moves every pin of it at once.
  `just setup`, and the pin check (`divbast-pin`) in each ci workflow
  that runs this repository's code, refuse workflows that call ci at more
  than one commit
- **divbast/frankenphp** builds and attests the binary,
  `ghcr.io/divbast/frankenphp:librespeed`, with its pins, LibreSpeed's
  extension list and the in-build checks on them
- **divbast/renovate-config** holds the presets `renovate.json` extends.
  `renovate.json` keeps only LibreSpeed's regex manager and its rule
- **divbast/infra** holds the org's settings, this repository's rulesets,
  the `divbast-renovate` App, the `renovate` environment and
  `LIBRESPEED_AUTOMERGE`

## Key commands

- `just setup` — fetch divbast/ci into `.ci/` at the commit the workflows
  pin, then install the pinned toolchain and PHPStan. One command on a
  fresh clone, and again whenever the pin moves; `just lint` fails, naming
  it, when `.ci` is not at the pinned commit (`ci-fresh`)
- `just build` — build the image locally. It does not verify the binary's
  attestation; `just ci-tools && .ci/bin/divbast-verify-frankenphp` does,
  with gh 2.102.0 or newer
- `just acceptance` — `go test ./test/acceptance/...` against the image
  `just build` wrote, under `ENGINE` (docker unless it says podman).
  `just IMAGE=… acceptance` tests another image. `IMAGE` is a plain value,
  so `IMAGE=… just acceptance` would quietly test the local one. An empty
  `IMAGE` fails rather than skipping. One test:
  `ENGINE=docker IMAGE=localhost/librespeed:dev mise exec -- go test -run
  TestRoutes ./test/acceptance/`
- `just test` — build, then the acceptance suite
- `just lint` — pinact, zizmor, actionlint, ShellCheck and shfmt, the Go
  gates (the compiler, `go vet`, `go fix -diff`, `go mod tidy -diff`,
  `golangci-lint fmt --diff` and `golangci-lint run`), `go-test`, `reuse
  lint`, hadolint, `docker build --check` and PHPStan. `go-test` runs every
  Go test under the race detector: `TestEnvDocs` and `TestUpstreamLayout`
  among them, which need no network and no image, while the acceptance
  suite skips when `IMAGE` is unset
- `just archive`, `just scan` — write the image as an OCI archive, then run
  `divbast-scan` over it and the published `:latest`, failing only on what
  the archive would add. Needs network access to ghcr.io
- `just actions-pin`, `just actions-update` — move the workflows' action pins
- `just ci` — every gate, locally. Run this before committing

## Where the toolchain comes from

- **`mise.toml` is where tool versions are written.** Go, golangci-lint,
  hadolint, ShellCheck, shfmt, actionlint, reuse, just, pinact, trivy and
  zizmor are all listed there. Every justfile recipe but `setup`, which
  installs them, runs through `mise exec`, so a recipe resolves those
  binaries and not a same-named one on PATH. The API token pinact and
  zizmor need is `GITHUB_TOKEN` alone, a fine-grained `github_pat_` token
  locally or CI's `ghs_`; no recipe falls back to `gh`. See the Local
  GitHub token section of divbast/ci's README
- **Go builds with the toolchain mise installs and nothing else.**
  `GOTOOLCHAIN=local` keeps `go.mod`'s line from downloading another. And
  `GOFLAGS=-mod=readonly`, because Composer's `vendor/` sits at the module
  root, where Go would take it for a Go vendor directory once `go.mod` has a
  `require`, and refuse to build; readonly is the default outside vendor
  mode, and still refuses to edit `go.mod`
- **`just lint` needs a running Docker daemon**, for `docker build --check`.
  `--check` is a buildx flag with no podman equivalent, so this one gate does
  not honour `ENGINE` and does not run without dockerd
- **PHP is a deliberate exception.** `php` and
  `composer` come from the host, not from mise: mise's only PHP backend
  compiles PHP from source, which is minutes added to every CI run in order to
  analyse a handful of files. PHPStan is therefore pinned by `composer.lock`,
  and `phpstan.neon` sets `phpVersion` so the *analysis target* is fixed no
  matter which PHP runs the analyser
- **So this repository carries `composer.json`, `composer.lock` and a
  `vendor/`, and that deserves an explanation**, because the image's whole
  claim is that no PHP toolchain reaches it. None of it does. The Containerfile
  copies named paths and never the build context, `vendor/` is gitignored, and
  `.dockerignore` excludes it too so that a `COPY` written broadly at some
  later date cannot quietly change the answer. `composer.json` has no `require`
  section, no `autoload` and no package name on purpose: Composer is asked to
  install a linter here, not to make this a PHP project
- Licensing for `composer.json`, `composer.lock` and `renovate.json` lives in
  `.license` sidecars. JSON has no comment syntax, and an SPDX tag written as
  a string value carries the closing quote into the expression, which `reuse`
  rejects

## Upstream facts — established from 6.3.1

- **No Composer and no build step.** `backend/geoip2.phar` and
  `backend/country_asn.mmdb` are checked in. The build is a checkout and a
  copy of the served subset
- **Zero egress without an API key.** `getIP.php` calls ipinfo.io only when
  `backend/getIP_ipInfo_apikey.php` sets a key; otherwise it answers from
  the mmdb through the phar
- **What upstream writes:** the SQLite database, PHP session files from
  `stats.php`, and `results/idObfuscation_salt.php`, inside the app tree,
  which `idObfuscation.php` names as `__DIR__.'/idObfuscation_salt.php'`. The
  image links that path to `/data`
- **Uploads are 20 MiB blobs** (`xhr_ul_blob_megabytes`, forced to 4 on
  Chrome mobile), sent by browsers with no `Content-Type`, and upstream asks
  for a `post_max_size` above them
- **PHP 8.5 changes into the script's directory**, and upstream's relative
  includes depend on it. The bootstrap changes into `results/` to run
  `getPdo()`, and back before the exec
- **`getPdo()` makes the database and its table** with `CREATE TABLE IF NOT
  EXISTS`, on the first result. `telemetry.php` reads its `$_POST` fields
  unchecked and records a row for any request, a GET included, so
  `site.caddy` routes it for POST alone
- **The modern UI links its share image as `/results/?id=…&style=modern`**,
  which relies on a directory index, so `site.caddy` routes `/results/` to
  `index.php`. Without `style=modern`, `index.php` draws the classic image
- **`json.php` prints only the ISP text**, so whether an address was
  redacted shows only in the stored row
- **Obfuscated ids are seven base-36 characters**; unobfuscated, the first
  row of a new database is `1`
- **`getIP.php` from loopback, with no headers**, answers exactly
  `{"processedString":"127.0.0.1","rawIspInfo":""}`, the probe's value
- **`stats.php`'s own login** is `$_POST['password'] === $stats_password`,
  and it lets in a session with `$_SESSION['logged'] === true`, which is
  what `login.php` sets. Its form posts to `stats.php?op=login`
- **The one PHP 8.5 deprecation is `imagedestroy`**, which goes unlogged
  with every other deprecation
- **PHP 8.5.12 fixes two RCEs**, so divbast/frankenphp's `PHP_VERSION`
  stays at or above it

## Code quality

- All linter gates are enforced as errors — fix them, don't suppress them. Any
  suppression, anywhere, needs an explicit, comment-level justification beside
  it to be accepted
- **The scan blocks only what a change would add, and is advisory while
  nothing is published.** `divbast-scan` scans the candidate and the
  published `:latest` for the same arch from one Trivy database, and fails
  only on a HIGH or CRITICAL finding, the same vulnerability in the same
  package, that the published image lacks. No release can make an advisory
  against something already shipped any worse, so blocking on one only
  holds back whatever else the release fixes. It is reported instead: on the
  run's summary page and, from main and the daily run, as a code scanning
  alert. Whether anything is published is the registry's to say, never
  Trivy's: the scan reads `:latest` as an anonymous client, and when ghcr.io
  refuses, asks GitHub why, with the check job's token, which is what its
  `packages: read` is for. Not published yet, or a package still private:
  every finding is reported, on the summary page, as a warning annotation
  and on stderr, and none blocks, because a first image would otherwise
  block on whatever the FrankenPHP binary already carries. Once the image
  is public, only new findings block. A public image the registry refuses,
  a registry that cannot be reached, or GitHub not answering fails the scan
  rather than waving it through. A local `just scan` never asks GitHub, so
  a refusal there is advisory, with a warning that only CI can tell what
  it means
- REUSE-compliant SPDX headers on every file; licence is **BSD-2-Clause**. The
  image's licences label also names what it ships from upstream:
  LGPL-3.0-or-later for LibreSpeed, CC-BY-SA-4.0 for the mmdb, Apache-2.0
  for the Open Sans fonts and OFL-1.1 for Inter. A shipped file under
  another licence changes the label and the README's Licences section
  together
- **The acceptance suite is Go, on divbast/ci's harness.** Every
  container it starts runs under `harness.Contract("16m")`, the *exact*
  confinement the contract claims, `--tmpfs /tmp:size=16m` included, so a
  check can never pass under looser settings than we ship. What CI proves
  is what ships. `TestContract` runs ci's contract suite against the image;
  the other tests are LibreSpeed's own, one topic per file, and run in
  parallel, apart from the restart tests, whose phases share one volume
  and so run in turn. Each serves through the harness's `Serve`, which
  publishes on the host's loopback at ports the engine picks, so no two
  tests, or two suites on one host, ask for the same one; the assertions
  (`StatusIs`, `Contains`, `Refuses` and the rest) are the harness's too,
  shared with every divbast image. What a test runs inside a container is a
  probe subcommand: ci's `core.php`, or LibreSpeed's own in
  `test/probe/app.php`, which PHPStan analyses at level max
- **Go is modern and idiomatic, and linted strictly**, under ci's
  `go/golangci.yml`. Bash is for what is short and will stay so: the
  justfile's recipes. A check that runs inside a Containerfile build stage
  is the exception, since a build stage has bash and no Go: it stays bash,
  under ShellCheck `enable=all`, shfmt and `set -euo pipefail`, with a Go
  test that runs it against fixtures and sees each way it can fail fail, as
  `test/upstreamlayout` does for `upstream-layout.sh`
- A test suite that has never failed proves nothing. When adding a check, prove
  it can fail before trusting it
