// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/divbast/ci/harness"
)

// dotSegmentClient is the client getIP.php names when it runs, as
// X-Forwarded-For carries it.
const dotSegmentClient = "203.0.113.9"

// TestDotSegments checks a path with a dot segment in it, raw or escaped,
// is routed by the path it resolves to. An allowed prefix must not reach
// past itself, a path that resolves onto an allowed route must run that
// route's script and no other, and one from an allowed route must not
// reach a script the allowlist leaves out.
func TestDotSegments(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{})

	// The routes match the path the file server would open, not the one
	// the client wrote; and neither reaches a script the allowlist leaves
	// out
	for _, path := range []string{
		"/frontend/../backend/geoip2.phar",
		"/frontend/%2e%2e/backend/geoip2.phar",
		"/images/..%2fbackend/country_asn.mmdb",
		"/backend/getIP.php/../getIP_util.php",
		"/backend/getIP.php/%2e%2e/getIP_util.php",
		"/backend/getIP.php/../../results/telemetry.php",
		"/backend/empty.php/%2e%2e/%2e%2e/results/stats.php",
	} {
		what := "GET " + path
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			harness.StatusIs(t, s, what, http.StatusNotFound, path)
		})
	}

	// A path that resolves onto an allowed route picks the route by that
	// path, and then FrankenPHP picks the script to run. That must be
	// getIP.php, known by its own answer, or there must be a refusal. A
	// helper such as getIP_util.php answers 200 with an empty body instead.
	for _, path := range []string{
		"/backend/getIP_util.php/../getIP.php",
		"/backend/getIP_util.php/%2e%2e/getIP.php",
		"/backend/getIP_util.php/%2E%2E/getIP.php",
		"/backend/getIP_util.php/.%2e/getIP.php",
		"/backend/getIP_ipInfo_apikey.php/../getIP.php",
		"/results/telemetry.php/../../backend/getIP.php",
		"/results/telemetry.php/%2e%2e/%2e%2e/backend/getIP.php",
		"/results/stats.php/../../backend/getIP.php",
		"/results/json.php/%2e%2e/%2e%2e/backend/getIP.php",
	} {
		t.Run("GET "+path, func(t *testing.T) {
			t.Parallel()
			runsOrRefused(t, s, path, "getIP.php", fmt.Sprintf(`{"processedString":%q,"rawIspInfo":""}`, dotSegmentClient))
		})
	}
}

// runsOrRefused checks GET path, sent exactly as written, either runs
// script, known by its answer, want, or is refused with a 400 or 404. Its
// name says which it saw.
func runsOrRefused(t *testing.T, s *harness.Serving, path, script, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	runs := fmt.Sprintf("GET %s runs %s", path, script)
	resp, err := s.Get(t.Context(), path, http.Header{"X-Forwarded-For": {dotSegmentClient}})
	switch {
	case err != nil:
		checks.True(runs, false, err.Error())
	case resp.Status == http.StatusOK:
		harness.Equal(checks, runs, want, string(resp.Body))
	case resp.Status == http.StatusBadRequest || resp.Status == http.StatusNotFound:
		checks.True(fmt.Sprintf("GET %s is refused", path), true, strconv.Itoa(resp.Status))
	default:
		checks.True(runs, false, fmt.Sprintf("got %d, which is neither %s's answer nor a refusal", resp.Status, script))
	}
}
