// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// garbageBytes is what garbage.php sends for ckSize=4: four chunks of 1 MiB.
const garbageBytes = 4 * 1048576

// TestSpeedTestEndpoints checks the speed test's download and getIP.php
// answer, the latter from the bundled database with no egress, and that
// the generated config is the one-server list and settings the UI reads.
func TestSpeedTestEndpoints(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{})

	t.Run("download", func(t *testing.T) {
		t.Parallel()
		checks := harness.NewChecks(t)
		const what = "GET /backend/garbage.php?ckSize=4: status, bytes"
		resp, err := s.Get(t.Context(), "/backend/garbage.php?ckSize=4", nil)
		if err != nil {
			checks.True(what, false, err.Error())
			return
		}
		harness.Equal(checks, what, fmt.Sprintf("%d %d", http.StatusOK, garbageBytes), fmt.Sprintf("%d %d", resp.Status, len(resp.Body)))
	})

	t.Run("getIP", func(t *testing.T) {
		t.Parallel()
		checks := harness.NewChecks(t)
		const loopback = "GET /backend/getIP.php over loopback"
		if body, err := loopbackGet(t.Context(), s.Probe, "/backend/getIP.php"); err != nil {
			checks.True(loopback, false, err.Error())
		} else {
			harness.Equal(checks, loopback, getIPLoopback, body)
		}

		// Answered from the bundled mmdb through the phar: there is no
		// egress
		const isp, resolves = "GET /backend/getIP.php?isp for X-Forwarded-For 1.1.1.1", "1.1.1.1 resolves to Cloudflare"
		resp, err := s.Get(t.Context(), "/backend/getIP.php?isp", http.Header{"X-Forwarded-For": {"1.1.1.1"}})
		if err != nil {
			checks.True(isp, false, err.Error())
			checks.True(resolves, false, err.Error())
			return
		}
		harness.Equal(checks, isp, http.StatusOK, resp.Status)
		var answer struct {
			ProcessedString string `json:"processedString"`
		}
		err = json.Unmarshal(resp.Body, &answer)
		processed := answer.ProcessedString
		checks.True(resolves, err == nil && (strings.Contains(processed, "Cloudflare") || strings.Contains(processed, "AS13335")),
			fmt.Sprintf("%q, from %s", processed, harness.Clip(string(resp.Body))))
	})

	checks := harness.NewChecks(t)
	list := fetch(t, s, "/server-list.json")
	var servers []json.RawMessage
	err := json.Unmarshal(list, &servers)
	checks.True("server-list.json is a one-element array", err == nil && len(servers) == 1, harness.Clip(string(list)))
	harness.Lacks(t, "server-list.json holds no absolute URL", "://", string(list))
	same := fetch(t, s, "/servers.json")
	checks.True("servers.json is the same list", len(list) > 0 && bytes.Equal(same, list), harness.Clip(string(same)))

	var settings map[string]any
	if body := fetch(t, s, "/settings.json"); json.Unmarshal(body, &settings) != nil {
		t.Errorf("settings.json is not a JSON object: %s", harness.Clip(string(body)))
	}
	harness.Equal(checks, "settings.json telemetry_level", "off", fmt.Sprint(settings["telemetry_level"]))
	harness.Equal(checks, "settings.json keeps upstream's test_order", "ID_U_P", fmt.Sprint(settings["test_order"]))
}

// fetch returns the body of GET path, from the host, whatever its status.
// A request that gets no answer fails t, and its body is empty.
func fetch(t *testing.T, s *harness.Serving, path string) []byte {
	t.Helper()
	resp, err := s.Get(t.Context(), path, nil)
	if err != nil {
		t.Errorf("GET %s: %v", path, err)
	}
	return resp.Body
}
