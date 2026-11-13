// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// healthProbe is the shipped probe HEALTHCHECK runs.
const healthProbe = "/usr/local/bin/librespeed-health"

// healthy is what /healthz answers when every check passes.
const healthy = "200 ok"

// TestHealthFromHost publishes the health port, which a deployment keeps
// unpublished, only to test it from outside: the contract's checks of the
// health port all run inside the container, so this is the one that
// shows the port answers from the host at all.
func TestHealthFromHost(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{port: portHealthOff, healthPort: portHealthOffHealth})
	healthzIs(t, s, "telemetry off: GET :8081/healthz", healthy)
}

// TestHealthFailures checks telemetry on a fresh volume is healthy before
// any result is posted, since the database the check needs is there from
// the start, and that each way the server can fail its users makes
// /healthz a 503 that names its check, and the probe exit 1. The check
// only reads: a database it found missing stays missing.
//
//nolint:paralleltest,tparallel // each phase restarts the server on the volume the one before it wrote, so they run in turn
func TestHealthFailures(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	volume := newVolume(t, e)
	sp := spec{port: portHealthOn, healthPort: portHealthOnHealth, volume: volume, env: telemetryOn()}

	t.Run("telemetry-on", func(t *testing.T) {
		s := serve(t, e, image, sp)
		checks := harness.NewChecks(t)
		ctx := t.Context()
		const fresh = "telemetry on, a fresh volume"
		tableIs(t, s.c, fresh+": no result has been recorded", emptyResultsTable)
		healthzIs(t, s, fresh+": GET :8081/healthz", healthy)
		if code, out, err := runHealthProbe(ctx, s.c); err != nil {
			checks.True(fresh+": "+healthProbe+" exits 0", false, err.Error())
		} else {
			checks.True(fresh+": "+healthProbe+" exits 0", code == 0, fmt.Sprintf("exit status %d: %s", code, harness.Clip(out)))
		}
		engineHealthy(t, e, image, s.c, fresh)

		// A database the server cannot write would take results and record
		// none of them
		chmodDB(t, s.c, 0o444)
		healthFails(t, s, "with /data/telemetry.db read-only", "database")
		chmodDB(t, s.c, 0o644)
		healthzIs(t, s, "writable again: GET :8081/healthz", healthy)
		harness.RunRecords(t, s.probe, "damage-db")
		healthFails(t, s, "after damage-db", "database")
		harness.RunRecords(t, s.probe, "remove-db")
		healthFails(t, s, "after remove-db", "database")
		// Asked, twice over, the check made no database, so a missing one
		// stays missing until upstream records a result
		harness.IsFile(t, s.c, "after remove-db and /healthz: /data/telemetry.db", "absent", telemetryDB)
	})

	t.Run("operator-ini", func(t *testing.T) {
		// PHP settings added from a mounted directory, as an operator might
		// add them, reach the server too, and every one the image depends
		// on fails the ini check. The bootstrap makes the database again, so
		// ini alone fails.
		phpOnVolume(t, e, image, volume, `mkdir("/data/php.d"); file_put_contents("/data/php.d/zz-operator.ini",
			"allow_url_fopen = On\ndisplay_errors = On\nlog_errors = Off\n");`)
		withINI := sp
		withINI.env = telemetryOn()
		withINI.env["PHP_INI_SCAN_DIR"] = ":/data/php.d"
		s := serve(t, e, image, withINI)
		body := healthFails(t, s, "with allow_url_fopen and display_errors on, and log_errors off", "ini")
		harness.Equal(harness.NewChecks(t), "the three settings, on one line, and no other check failed",
			"ini: allow_url_fopen is on, display_errors is on, log_errors is off", body)
	})
}

// healthz sends GET /healthz to s's published health port, and returns
// the status and the body, less its final newline.
func healthz(ctx context.Context, s *served) (status int, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.healthBase+"/healthz", http.NoBody)
	if err != nil {
		return 0, "", err
	}
	resp, err := send(harness.HostClient(), req)
	if err != nil {
		return 0, "", err
	}
	return resp.status, strings.TrimSuffix(string(resp.body), "\n"), nil
}

// healthzIs checks GET :8081/healthz, from the host, answers want, its
// status and body as one.
func healthzIs(t *testing.T, s *served, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	status, body, err := healthz(t.Context(), s)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, fmt.Sprintf("%d %s", status, body))
}

// healthFails checks /healthz is 503 and names the failed check on a line
// of its own, and that the probe exits 1, naming the /healthz half and
// that check. The getIP.php half, still sound, goes unnamed. It returns
// the body of the 503.
func healthFails(t *testing.T, s *served, what, check string) string {
	t.Helper()
	checks := harness.NewChecks(t)
	ctx := t.Context()
	status, body, err := healthz(ctx, s)
	if err != nil {
		checks.True(what+": GET :8081/healthz", false, err.Error())
		checks.True(fmt.Sprintf("%s: /healthz names %s", what, check), false, err.Error())
	} else {
		harness.Equal(checks, what+": GET :8081/healthz", http.StatusServiceUnavailable, status)
		named := slices.ContainsFunc(strings.Split(body, "\n"), func(line string) bool { return strings.HasPrefix(line, check+":") })
		checks.True(fmt.Sprintf("%s: /healthz names %s", what, check), named, strings.ReplaceAll(body, "\n", " | "))
	}

	exits := fmt.Sprintf("%s: %s exits 1", what, healthProbe)
	only := fmt.Sprintf("%s: the probe names the /healthz half and %s, and only that half", what, check)
	code, out, err := runHealthProbe(ctx, s.c)
	if err != nil {
		checks.True(exits, false, err.Error())
		checks.True(only, false, err.Error())
		return body
	}
	harness.Equal(checks, exits, 1, code)
	half := strings.Index(out, "http://127.0.0.1:8081/healthz")
	checks.True(only, half >= 0 && strings.Contains(out[half:], check+":") && !strings.Contains(out, "getIP.php"), harness.Clip(out))
	return body
}

// runHealthProbe runs the shipped probe in c, as HEALTHCHECK runs it, and
// returns its exit status and all it printed.
func runHealthProbe(ctx context.Context, c *harness.Container) (code int, out string, err error) {
	res, err := c.Exec(ctx, nil, healthCmd...)
	if err != nil {
		return 0, "", err
	}
	return res.Code, strings.TrimSpace(string(res.Stdout) + string(res.Stderr)), nil
}

// engineHealthy checks the engine's own verdict, which it reaches on its
// own schedule, as it would for a user: Docker runs the image's
// HEALTHCHECK, and Podman the --health-cmd it was given, from the systemd
// timer it sets up for it. A shell-form command needs /bin/sh, which the
// image lacks, so only the exec form can pass. The engine must say healthy
// within the image's start period and three of its intervals, the window
// the contract's own engine-healthy check gives it, and under that check's
// name.
func engineHealthy(t *testing.T, e harness.Engine, image string, c *harness.Container, scenario string) {
	t.Helper()
	ctx := t.Context()
	var startPeriod time.Duration
	if cfg, err := e.ImageConfig(ctx, t, image); err != nil {
		t.Errorf("reading the image's HEALTHCHECK: %v", err)
	} else if cfg.Healthcheck != nil {
		startPeriod = cfg.Healthcheck.StartPeriod
	}
	found := "healthy"
	if err := c.EngineHealthy(ctx, startPeriod); err != nil {
		found = err.Error()
	}
	harness.Equal(harness.NewChecks(t), scenario+": the engine reports the container healthy within the start period and three intervals",
		"healthy", found)
}

// chmodDB sets the mode of the database, as the server's own uid, which
// owns it, or fails t.
func chmodDB(t *testing.T, c *harness.Container, mode int) {
	t.Helper()
	if _, err := harness.PHPIn(t.Context(), c, fmt.Sprintf(`if (!chmod(%q, 0%s)) { exit(1); }`, telemetryDB, strconv.FormatInt(int64(mode), 8))); err != nil {
		t.Fatalf("chmod %o %s: %v", mode, telemetryDB, err)
	}
}
