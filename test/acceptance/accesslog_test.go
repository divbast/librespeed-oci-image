// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// The request the shipped probe sends, every 30 s, and the address it
// sends it from: the container's own loopback.
const (
	probePath = "/backend/getIP.php"
	probeFrom = "127.0.0.1"
)

// accessEntry is the part of one line of the app port's access log a test
// reads.
type accessEntry struct {
	Request struct {
		RemoteIP string `json:"remote_ip"`
		Method   string `json:"method"`
		URI      string `json:"uri"`
	} `json:"request"`
}

// String is the entry as a check's detail shows it.
func (a accessEntry) String() string {
	return fmt.Sprintf("%s %s from %s", a.Request.Method, a.Request.URI, a.Request.RemoteIP)
}

// TestProbeUnlogged checks the shipped probe's request leaves no line in
// the access log: thousands a day would bury the lines worth reading.
// Every other request is logged as before: the same request from
// anywhere but the container's loopback, and a browser's, which always
// carries a query, from the loopback too (LS-18).
func TestProbeUnlogged(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	// A network of the test's own, so this host's requests come from an
	// address on it, never from the container's loopback
	s := serve(t, e, image, spec{network: newNetwork(t, e)})
	checks := harness.NewChecks(t)
	ctx := t.Context()

	code, out, err := runHealthProbe(ctx, s.C)
	if err != nil {
		t.Fatal(err)
	}
	harness.Equal(checks, healthProbe+" exits 0", 0, code)
	if code != 0 {
		t.Logf("%s: %s", healthProbe, out)
	}
	harness.StatusIs(t, s, "GET "+probePath+" from this host", http.StatusOK, probePath)
	const browser = probePath + "?isp=true&distance=km&r=0.5"
	if _, gerr := loopbackGet(ctx, s.Probe, browser); gerr != nil {
		checks.True("GET "+browser+" from the loopback", false, gerr.Error())
	}

	// Lines land a moment after their answers, in the order they were
	// answered, so once both of the later ones are in, the probe's would be
	fromHost := func(a accessEntry) bool {
		return a.Request.URI == probePath && a.Request.RemoteIP != probeFrom
	}
	fromBrowser := func(a accessEntry) bool {
		return a.Request.URI == browser && a.Request.RemoteIP == probeFrom
	}
	entries, err := accessLogUntil(ctx, s.C, fromHost, fromBrowser)
	if err != nil {
		checks.True("the access log holds GET "+probePath+" from this host", false, err.Error())
		checks.True("the access log holds GET "+browser+" from the loopback", false, err.Error())
		return
	}
	checks.True("the access log holds GET "+probePath+" from this host", slices.ContainsFunc(entries, fromHost), shown(entries))
	checks.True("the access log holds GET "+browser+" from the loopback", slices.ContainsFunc(entries, fromBrowser), shown(entries))
	probes := slices.DeleteFunc(slices.Clone(entries), func(a accessEntry) bool {
		return a.Request.URI != probePath || a.Request.RemoteIP != probeFrom
	})
	checks.True("the access log holds no GET "+probePath+" from "+probeFrom+", the probe's", len(probes) == 0, shown(probes))
}

// accessLogUntil returns c's access log, once it holds a line that each of
// want matches, or within accessLogWithin whatever it holds then.
func accessLogUntil(ctx context.Context, c *harness.Container, want ...func(accessEntry) bool) ([]accessEntry, error) {
	deadline := time.Now().Add(accessLogWithin)
	for {
		logs, err := c.Logs(ctx)
		if err != nil {
			return nil, err
		}
		var entries []accessEntry
		for line := range strings.Lines(logs) {
			var a accessEntry
			if json.Unmarshal([]byte(line), &a) == nil && a.Request.URI != "" {
				entries = append(entries, a)
			}
		}
		all := !slices.ContainsFunc(want, func(w func(accessEntry) bool) bool { return !slices.ContainsFunc(entries, w) })
		if all || !time.Now().Before(deadline) {
			return entries, nil
		}
		if err := harness.Pause(ctx, time.Second); err != nil {
			return nil, err
		}
	}
}

// shown lists entries for a check's detail.
func shown(entries []accessEntry) string {
	lines := make([]string, 0, len(entries))
	for _, a := range entries {
		lines = append(lines, a.String())
	}
	return fmt.Sprintf("%d line(s): %s", len(entries), strings.Join(lines, " | "))
}
