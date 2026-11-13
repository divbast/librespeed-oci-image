// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// statsPasswordValue is the STATS_PASSWORD the tests set.
const statsPasswordValue = "correct-horse"

// statsPath is upstream's stats page.
const statsPath = "/results/stats.php"

// statsPaths is every way a client might write a login to stats.php. PHP
// takes the last of a repeated key, and Caddy's query matcher matches any
// of them. PHP also drops a key's leading spaces and cuts it at a NUL,
// while Caddy matches a key as written and refuses to match at all on a
// query it cannot parse: a stray ; or a bad escape. So several of these
// say op=login to PHP but not to Caddy.
var statsPaths = []string{
	"/results/stats.php",
	"/results/stats.php?op=login",
	"/results/stats.php?op=x&op=login",
	"/results/stats.php?op=login&op=x",
	"/results/stats.php?op=%6cogin",
	"/results/stats.php?%6fp=login",
	"/results/stats.php?OP=login",
	"/results/stats.php?op=LOGIN",
	"/results/stats.php?op%00=login",
	"/results/stats.php?%20op=login",
	"/results/stats.php?+op=login",
	"/results/stats.php?x=;&op=login",
	"/results/stats.php?op=login&x=%zz",
	"/results/stats.php?op[]=login",
	"/RESULTS/STATS.PHP?op=login",
	"/results/./stats.php?op%00=login",
	"/results//stats.php?%20op=login",
	"/results/%73tats.php?op%00=login",
	"/results/json.php/../stats.php?op=login",
	"/results/json.php/%2e%2e/stats.php?op%00=login",
	"/backend/getIP.php/../../results/stats.php?x=;&op=login",
}

// statsExits are paths that start at stats.php and resolve to another
// script, each beside that script's own path. Raw, such a path would split
// at its first .php and run stats.php, with whatever was posted to it.
var statsExits = []struct{ path, target string }{
	{"/results/stats.php/../../backend/getIP.php?op=login", "/backend/getIP.php?op=login"},
	{"/results/stats.php/%2e%2e/%2e%2e/backend/getIP.php?op=login", "/backend/getIP.php?op=login"},
	{"/results/stats.php/..%2f..%2fbackend/getIP.php?op=login", "/backend/getIP.php?op=login"},
	{"/RESULTS/STATS.PHP/../../backend/getIP.php?op=login", "/backend/getIP.php?op=login"},
	{"/results/stats.php/.%2e/.%2e/backend/empty.php?op=login", "/backend/empty.php?op=login"},
	{"/results/stats.php/../json.php?op=login&id=1", "/results/json.php?op=login&id=1"},
	{"/results/stats.php/%2e%2e/json.php?op=login&id=1", "/results/json.php?op=login&id=1"},
	{"/results/stats.php/../../results/json.php?op=login&id=1", "/results/json.php?op=login&id=1"},
	{"/results/stats.php/../telemetry.php?op=login", "/results/telemetry.php?op=login"},
	{"/results/stats.php/%2E%2E/telemetry.php?op=login", "/results/telemetry.php?op=login"},
	{"/results/stats.php/../sanitycheck.php?op=login", "/results/sanitycheck.php?op=login"},
}

// statsTitle is in upstream's stats page and form, and in every answer of
// login.php.
const statsTitle = "<title>LibreSpeed - Stats</title>"

// recordedID is the id telemetry.php hands out, which differs from one
// request to the next.
var recordedID = regexp.MustCompile(`(?m)^id \d*$`)

// exitRequest sends method path from the host as leavesStats sends it,
// with the password posted when the method is POST, and returns the body,
// with any id telemetry.php hands out made N, then the status, on a line
// of its own.
func exitRequest(t *testing.T, s *harness.Serving, method, path, password string) (string, error) {
	t.Helper()
	header := http.Header{"X-Forwarded-For": {dotSegmentClient}}
	var (
		resp harness.Response
		err  error
	)
	if method == http.MethodPost {
		resp, err = postForm(t.Context(), s, harness.HostClient(), path, header, url.Values{"password": {password}})
	} else {
		resp, err = s.Get(t.Context(), path, header)
	}
	if err != nil {
		return "", err
	}
	return recordedID.ReplaceAllString(string(resp.Body), "id N") + "\n" + strconv.Itoa(resp.Status), nil
}

// leavesStats checks a path out of stats.php is answered by the script it
// resolves to, exactly as that script answers when asked directly, or is
// refused. Neither stats.php nor its login ever answers it.
func leavesStats(t *testing.T, s *harness.Serving, method, path, target, password string) {
	t.Helper()
	checks := harness.NewChecks(t)
	runs := fmt.Sprintf("%s %s runs %s", method, path, strings.SplitN(target, "?", 2)[0])
	got, err := exitRequest(t, s, method, path, password)
	if err != nil {
		checks.True(runs, false, err.Error())
		return
	}
	status := got[strings.LastIndexByte(got, '\n')+1:]
	switch {
	case strings.Contains(got, statsTitle):
		checks.True(runs, false, fmt.Sprintf("stats.php or its login answered (%s)", status))
	case status == "400" || status == "404":
		checks.True(fmt.Sprintf("%s %s is refused", method, path), true, status)
	default:
		want, err := exitRequest(t, s, method, target, password)
		if err != nil {
			checks.True(runs, false, err.Error())
			return
		}
		harness.Equal(checks, runs, want, got)
	}
}

// TestStatsWithoutPassword checks that with telemetry on and no
// STATS_PASSWORD, neither stats.php nor the login answers, however either
// is asked for, and no path that starts at stats.php runs it on its way
// elsewhere. Set while telemetry is off, STATS_PASSWORD is named in the
// log and ignored, and one bcrypt would cut short stops the start.
func TestStatsWithoutPassword(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)

	t.Run("telemetry-on", func(t *testing.T) {
		t.Parallel()
		s := serve(t, e, image, spec{volume: newVolume(t, e), env: telemetryOn()})
		form := url.Values{"password": {statsPasswordValue}}
		for _, path := range append(statsPaths[:len(statsPaths):len(statsPaths)], "/results/login.php", "/login.php") {
			harness.StatusIs(t, s, "GET "+path, http.StatusNotFound, path)
			resp, err := postForm(t.Context(), s, harness.HostClient(), path, nil, form)
			harness.Answers(t, "POST "+path, http.StatusNotFound, resp, err)
		}
		for _, exit := range statsExits {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				leavesStats(t, s, method, exit.path, exit.target, statsPasswordValue)
			}
		}
		harness.IsFile(t, s.C, "no /data/stats-password.hash", "absent", statsPasswordHash)
	})

	t.Run("telemetry-off", func(t *testing.T) {
		t.Parallel()
		s := serve(t, e, image, spec{env: map[string]string{"STATS_PASSWORD": statsPasswordValue}})
		harness.LogContains(t, s.C, "STATS_PASSWORD without TELEMETRY: the log says it is ignored",
			"TELEMETRY is off, so these are ignored: STATS_PASSWORD")
		harness.LogLacks(t, s.C, "STATS_PASSWORD without TELEMETRY: the log never shows it", statsPasswordValue)
		harness.StatusIs(t, s, "GET /results/stats.php with only STATS_PASSWORD set", http.StatusNotFound, statsPath)
		resp, err := postForm(t.Context(), s, harness.HostClient(), statsPath+"?op=login", nil,
			url.Values{"password": {statsPasswordValue}})
		harness.Answers(t, "POST /results/stats.php?op=login with only STATS_PASSWORD set", http.StatusNotFound, resp, err)
	})

	t.Run("too-long", func(t *testing.T) {
		t.Parallel()
		// bcrypt reads the first 72 bytes and ignores the rest
		harness.Refuses(t, e, refusing(image, map[string]string{"STATS_PASSWORD": strings.Repeat("0", 73)}), refusedExit,
			"a STATS_PASSWORD of 73 bytes", "STATS_PASSWORD is 73 bytes long")
	})
}

// statsPasswordHash is what login.php checks a password against.
const statsPasswordHash = dataDir + "/stats-password.hash"
