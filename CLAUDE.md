<!-- SPDX-License-Identifier: BSD-2-Clause -->
<!-- SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee> -->

# librespeed-oci-image

A [LibreSpeed](https://github.com/librespeed/speedtest) speed test container
image that runs at zero Linux capabilities on a read-only root filesystem as
a non-root user.

Published as `ghcr.io/divbast/librespeed`, from the divbast organisation, whose
settings, rulesets and Renovate wiring live in `divbast/infra`.

The pipeline (the workflows, the justfile, `mise.toml`, the lint configs, the
test harness and `renovate.json`) is a deliberate copy of
`divbast/baikal-oci-image`'s, to be replaced by divbast's shared workflows.

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
  acceptance suite passes the same array under Podman, and checks the
  image's `HEALTHCHECK` under Docker only
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
  keep the order they are written in. Section 5's dot-segment checks hold
  each route to the script it names; FrankenPHP 1.12.7 fails them, so a
  builder that brings back its raw-path split goes red
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
- **Every variable is read through `envBool` or `envString` with a literal
  name, and is in the README's Env Vars table.** `test/env-docs.sh`, in
  `just lint`, fails otherwise, and fails on any read whose name is not
  written where it is read. The switches take exactly `true` or `false`, and
  every variable is checked before anything is written, so a typo stops the
  start instead of meaning the default
- **`test/upstream-layout.sh` guards every upstream marker the image relies
  on**: each file it serves or requires, each string `renderIndex` replaces,
  `stats.php`'s login checks, `idObfuscation.php`'s salt path and the empty
  `$IPINFO_APIKEY`. The fetch stage runs it, so a release that moves or
  rewords one fails the build instead of shipping. A marker the image starts
  to rely on goes into the guard in the same change
- **Nothing listens but 8080 and 8081.** Caddy's admin API would otherwise
  listen on 127.0.0.1:2019 and take a new config, with no credentials, from
  anything sharing the container's network: under host networking, every
  process on the host. Section 2 reads the kernel's socket tables and fails
  on any other port a process in the container listens on
- **One process tree, always.** FrankenPHP is a single binary containing PHP and
  Caddy, and the entrypoint `pcntl_exec`s it so it inherits PID 1. A dead PHP kills
  the container. Do not add a supervisor, s6, or a second long-running process
- **Zero egress.** `allow_url_fopen` is off, because `getIP.php` would call
  ipinfo.io through a URL stream if a key were ever set; upstream's
  `$IPINFO_APIKEY` must stay empty, and the layout guard fails the build if
  it is not; ISP names come from the bundled mmdb. The stability test's
  external targets are fetched by the visitor's browser, never the
  container. Section 3 proves it from a genuinely `--internal` network
- **Nothing may add a shell or a package manager to the final image.** The base is
  distroless; that is the point. What it removes is us running apt, not the
  patching itself - `cc-debian13` still ships libc6, libssl3t64, libstdc++6,
  libgcc-s1, libgomp1, libzstd1 and zlib1g, all executable code that takes CVEs.
  That obligation moves to the base image's own rebuilds — and **nothing in
  this repository collects them on its own.** Rebuilding an unchanged
  Containerfile does not pick them up. **Renovate is the update path**: it runs
  every morning, after every merge to main and whenever CI finishes on one
  of its branches, from
  `.github/workflows/renovate.yml`, as the org's `divbast-renovate` App,
  managed from `divbast/infra`. After a merge it rebases every
  open PR left behind main, since GitHub re-runs no checks when a base moves
  and a green check on a stale base proves nothing. **It merges its own
  PRs** once every check passes, with a squash, while the repository
  variable `LIBRESPEED_AUTOMERGE` is `enabled`; any other value stops merging
  from the next run. **A merge never publishes.** CI's daily run is the
  publishing slot: it publishes main when main moved since the commit the
  published `:latest` was built from, so users see at most one new image a
  day, and a manual run of `build` publishes at once for an urgent fix.
  Neither ever moves `:latest` backwards (`test/publish-decision.sh`): an
  older commit would take back whatever the newer image fixed. The daily run
  also catches upstream breakage early — a LibreSpeed tag that moved, a
  builder that changed behaviour — and a new advisory against what is
  already shipped surfaces there as a code scanning alert, not a red run.
  `.github/workflows/stall.yml` fails each day that updates have stopped
  flowing: a Renovate PR stuck, Renovate silent, or two slots lost in a row
- **Every input is pinned in the Containerfile**, not resolved at build time.
  LibreSpeed releases carry no assets, only GitHub's generated archives,
  which are not byte-stable, so `LIBRESPEED_VERSION` sits directly above
  `LIBRESPEED_COMMIT`, Renovate's regex moves the two in one PR, and the
  fetch stage checks out the commit and fails if the tag no longer names it.
  The two ARGs must stay adjacent and in that order. The builder, runtime and
  fetch-stage images are pinned by digest. For the binary, `PHP_VERSION`,
  `CADDY_VERSION` and the three plugin ARGs fix what `build-static.sh` and
  xcaddy would otherwise take as newest; Renovate moves the builder digest,
  Caddy and the plugins together as one `frankenphp` group; and
  `test/pinned-versions.sh` fails the build if the binary's own build info
  disagrees with any pin. spc is built from `SPC_COMMIT` in place of the
  nightly download, and `test/pinned-spc.sh` fails the build if anything
  else ran `spc craft`. **What spc downloads is still chosen at build time:**
  several libraries, openssl and zlib among them, come from the newest
  release that matches. `PHP_EXTENSIONS` names exactly what the binary
  loads, and `test/extensions.sh` fails the build if the two disagree or if
  gd lacks FreeType or PNG, which the share image needs
- **The binary must never carry a file capability.** The official FrankenPHP build
  carries `cap_net_bind_service=ep`, and a binary with a file capability cannot be
  exec'd at all under `--cap-drop ALL` — it dies with `Operation not permitted`
  and exit 126 before PHP starts
- **`post_max_size` stays under 16 KiB.** PHP keeps a body it reads in
  memory up to 16 KiB and spools the rest into `/tmp`. At `8K` it reads none
  of the speed test's 20 MiB uploads, so `/tmp` stays as it is under any
  load and 16 MiB is enough. Upstream's `32M` cut even a single upload short
  under the 16 MiB tmpfs while the client still saw a 200 with every byte
  sent, which is why section 7 checks what the server read, from the access
  log. It follows that no form may outgrow 8 KiB (sections 11 and 14 assert
  the telemetry record and the login fit), that `telemetry_level` stays
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
  answered before it can take that lock
- **The client the limit counts is Caddy's `{client_ip}`**: the connection's
  address, or with `TRUSTED_PROXIES`, `X-Forwarded-For` read right to left
  and strict. A range with a prefix of 0 is refused: every client of its
  family would be a trusted proxy, free to write an address of the other
  family, which the range does not hold, and be counted as that. `REMOTE_ADDR` and upstream's own address handling stay as they
  are: `getIP_util.php` trusts `Client-IP`, `X-Real-IP`, `X-Forwarded-For`
  and `CF-Connecting-IPv6` from anyone, and the image leaves that alone.
  Rootless Podman hands a container on a user-defined network every
  connection to a published port from the container's own address, so the
  acceptance suite fixes the stats server's address, or a restart would face
  a new client
- **The application tree stays root-owned and unwritten.** What changes per
  start lives in `/tmp/librespeed`, mode 0700. What outlives a start lives in
  `/data`: `telemetry.db`, `idObfuscation_salt.php`, `stats-password.hash`
  (rewritten on every start with `TELEMETRY=true` and `STATS_PASSWORD` set)
  and `stats-login.db`
- **Never rewrite the obfuscation salt.** Every share link handed out was
  made with it. It is created once, when telemetry and obfuscation are both
  on and it is absent, and with `telemetry.db` it is one restore unit
- **The results database is made at start, by upstream's own `getPdo()`**,
  so its schema is always upstream's and `/healthz`'s `database` check passes
  on a fresh volume. The start is refused when `/data` is not writable, when
  an existing database or salt is not usable by uid 65532, or when `getPdo()`
  cannot open the database. Served, such a database would answer every
  result with an empty 200 and record none, and upstream reads a salt it
  cannot read as 0, which changes every id. `/healthz` opens the database
  read-only and never creates it

## Key commands

- `just setup` — install the pinned toolchain. One command on a fresh clone
- `just build` — build the image locally
- `just test` — build, then run the acceptance suite against it
- `just lint` — pinact, zizmor, hadolint, `docker build --check`, ShellCheck,
  `reuse lint`, PHPStan and `test/env-docs.sh`
- `just scan` — Trivy over the image archive `just archive` writes and over
  the published `:latest`, failing only on what the archive would add. Needs
  network access to ghcr.io
- `just actions-pin`, `just actions-update` — move the workflow's action pins
- `just ci` — every gate, locally. Run this before committing

## Where the toolchain comes from

- **`mise.toml` is where tool versions are written.** hadolint, ShellCheck,
  reuse, just, jq, pinact, trivy and zizmor are all listed there. Every justfile
  recipe runs through `mise exec`, so a recipe resolves those binaries and not a
  same-named one on PATH. `gh` is the exception, and only as a local fallback
  for the API token pinact, zizmor and spc's build need; CI passes its own
- **`just lint` needs a running Docker daemon**, for `docker build --check`.
  `--check` is a buildx flag with no podman equivalent, so this one gate does
  not honour `ENGINE` and does not run without dockerd
- **PHP is a deliberate exception.** `php` and
  `composer` come from the host, not from mise: mise's only PHP backend
  compiles PHP from source, which is minutes added to every CI run in order to
  analyse a handful of files. PHPStan is therefore pinned by `composer.lock`,
  and `phpstan.neon` sets `phpVersion` so the *analysis target* is fixed no
  matter which PHP runs the analyser. `test/env-docs.sh` uses the host's PHP
  too, for its tokenizer
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
  unchecked and records a row for any request, a GET included
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
- **PHP 8.5.12 fixes two RCEs**, so `PHP_VERSION` stays at or above it

## Code quality

- All linter gates are enforced as errors — fix them, don't suppress them. Any
  suppression, anywhere, needs an explicit, comment-level justification beside
  it to be accepted
- **The scan blocks only what a change would add, and is advisory while
  nothing is published.** `test/scan.sh` scans the candidate and the
  published `:latest` for the same arch from one Trivy database, and fails
  only on a HIGH or CRITICAL finding, the same vulnerability in the same
  package, that the published image lacks. No release can make an advisory
  against something already shipped any worse, so blocking on one only
  holds back whatever else the release fixes. It is reported instead: on the
  run's summary page and, from main and the daily run, as a code scanning
  alert. "Nothing published" means the registry answers not-found or denied
  to an anonymous pull of `:latest`, so a package still private counts as
  unpublished. Then every finding is reported, on the summary page, as a
  warning annotation and on stderr, and none blocks, because a first image
  would otherwise block on whatever the FrankenPHP binary already carries.
  Once the image is public, only new findings block. A registry that cannot
  be reached fails the scan rather than waving it through
- REUSE-compliant SPDX headers on every file; licence is **BSD-2-Clause**. The
  image's licences label also names what it ships from upstream:
  LGPL-3.0-or-later for LibreSpeed, CC-BY-SA-4.0 for the mmdb, Apache-2.0
  for the Open Sans fonts and OFL-1.1 for Inter. A shipped file under
  another licence changes the label and the README's Licences section
  together
- The acceptance suite runs the image under the *exact* confinement the contract
  claims, `--tmpfs /tmp:size=16m` included, so a check can never pass under
  looser settings than we ship. What CI proves is what ships
- A test suite that has never failed proves nothing. When adding a check, prove
  it can fail before trusting it
