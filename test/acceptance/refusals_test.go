// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
	"github.com/divbast/librespeed-oci-image/test/probe"
)

// refuseWithin bounds how long a container that must refuse to start may
// run before it exits.
const refuseWithin = 30 * time.Second

// TestBadTelemetryRefused checks a TELEMETRY that is neither true nor
// false stops the start, and the log names it.
func TestBadTelemetryRefused(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()
	checks := harness.NewChecks(t)

	const exits, names = "TELEMETRY=maybe: the container exits", "TELEMETRY=maybe: the log names TELEMETRY"
	c, err := e.Start(ctx, t, &harness.RunSpec{
		Image:   image,
		Network: "none",
		Confine: harness.Contract(tmpfsSize),
		Env:     map[string]string{"TELEMETRY": "maybe"},
	})
	if err != nil {
		checks.True(exits, false, err.Error())
		checks.True(names, false, err.Error())
		return
	}
	if code, werr := c.WaitExit(ctx, refuseWithin); werr != nil {
		checks.True(exits, false, werr.Error())
	} else {
		harness.Equal(checks, exits, 1, code)
	}
	logs, err := c.Logs(ctx)
	if err != nil {
		checks.True(names, false, err.Error())
		return
	}
	contains(t, names, "TELEMETRY", logs)
}

// bootstrapRun is what the probe's bootstrap-with prints: the bootstrap's
// exit status, null if it was still running at the deadline, and its
// stderr.
type bootstrapRun struct {
	Exit   *int   `json:"exit"`
	Stderr string `json:"stderr"`
}

// exit says how the bootstrap ended: its exit status, or that it was
// still running, and so had started the server, when the probe stopped it.
func (r bootstrapRun) exit() string {
	if r.Exit == nil {
		return "still running at the deadline, so it was stopped"
	}
	return strconv.Itoa(*r.Exit)
}

// TestInvalidUTF8Refused checks the bootstrap refuses a TITLE, TAGLINE or
// GDPR_EMAIL that is not UTF-8, which htmlspecialchars() would make
// empty without a word, before it writes anything. No engine's -e can
// carry such bytes, so the probe runs the bootstrap with them, in a
// container that only sleeps and so has nothing written yet.
func TestInvalidUTF8Refused(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	ctx := t.Context()
	checks := harness.NewChecks(t)

	c, err := e.Start(ctx, t, &harness.RunSpec{
		Image:      image,
		Network:    "none",
		Confine:    harness.Contract(tmpfsSize),
		Entrypoint: []string{harness.FrankenPHP},
		Args:       []string{"php-cli", "-r", "sleep(600);"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := harness.InstallProbe(ctx, c, probe.App)
	if err != nil {
		t.Fatal(err)
	}

	// Latin-1 for "été"
	const latin1 = "e974e9"
	for _, name := range []string{"TITLE", "TAGLINE", "GDPR_EMAIL"} {
		exits, says := name+" in Latin-1: the bootstrap exits", name+" in Latin-1: it says so"
		run, rerr := bootstrapWith(t, p, map[string]string{name: latin1})
		if rerr != nil {
			checks.True(exits, false, rerr.Error())
			checks.True(says, false, rerr.Error())
			continue
		}
		harness.Equal(checks, exits, "1", run.exit())
		contains(t, says, name+" is not valid UTF-8", run.Stderr)
	}

	// The same word in UTF-8 passes, and the start stops at the next
	// problem
	const goesOn, accepted = "TITLE in UTF-8 passes, and the start goes on", "TITLE in UTF-8 is not refused"
	run, err := bootstrapWith(t, p, map[string]string{
		"TITLE":           "c3a974c3a9",
		"TRUSTED_PROXIES": fmt.Sprintf("%x", "not-a-cidr"),
	})
	if err != nil {
		checks.True(goesOn, false, err.Error())
		checks.True(accepted, false, err.Error())
	} else {
		contains(t, goesOn, "TRUSTED_PROXIES holds 'not-a-cidr'", run.Stderr)
		checks.True(accepted, !strings.Contains(run.Stderr, "UTF-8"), clip(run.Stderr))
	}

	const nothing = "every refusal came before the bootstrap wrote anything"
	written, err := phpIn(ctx, c, `echo is_dir("/tmp/librespeed") ? "present" : "absent";`)
	if err != nil {
		checks.True(nothing, false, err.Error())
		return
	}
	harness.Equal(checks, nothing, "absent", written)
}

// bootstrapWith runs the probe's bootstrap-with in p, with each name set
// to the bytes its hex value spells.
func bootstrapWith(t *testing.T, p *harness.Probe, hexValues map[string]string) (bootstrapRun, error) {
	t.Helper()
	assignments, err := json.Marshal(hexValues)
	if err != nil {
		return bootstrapRun{}, err
	}
	res, err := p.Output(t.Context(), "bootstrap-with", string(assignments))
	if err != nil {
		return bootstrapRun{}, err
	}
	if res.Code != 0 {
		return bootstrapRun{}, fmt.Errorf("bootstrap-with %s: exit status %d: %s%s", assignments, res.Code, res.Stdout, res.Stderr)
	}
	var run bootstrapRun
	if err := json.Unmarshal(res.Stdout, &run); err != nil {
		return bootstrapRun{}, fmt.Errorf("bootstrap-with %s printed no JSON object: %w: %s", assignments, err, clip(string(res.Stdout)))
	}
	return run, nil
}
