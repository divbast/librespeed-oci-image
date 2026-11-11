// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"net/http"
	"testing"

	"github.com/divbast/ci/harness"
)

// TestTelemetryOff checks telemetry is off by default: no results route
// answers, directly or by way of a dot segment from an allowed route, and
// nothing telemetry needs is generated. Its own switches, set while it is
// off, are named in the log and ignored, and the image's read-only /data
// is not touched for a salt; but a value that is neither true nor false
// still stops the start.
func TestTelemetryOff(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		// As a user who only runs it would: no volume, and nothing set
		s := serve(t, e, image, spec{port: portTelemetryOff})
		for _, path := range []string{
			"/results/telemetry.php", "/results/json.php", "/results/index.php", "/results/",
			"/results/json.php?id=1", "/results/?id=1&style=modern",
		} {
			statusIs(t, s, "GET "+path, http.StatusNotFound, path)
		}
		body, contentType := multipartForm([][2]string{{"dl", resultDL}})
		resp, err := s.do(t.Context(), harness.HostClient(), http.MethodPost, "/results/telemetry.php",
			http.Header{"Content-Type": {contentType}}, body)
		answers(t, "POST /results/telemetry.php", http.StatusNotFound, resp, err)
		// Nor do they answer from an allowed route by way of a dot segment
		for _, path := range []string{
			"/backend/getIP.php/../../results/json.php", "/backend/getIP.php/%2e%2e/%2e%2e/results/index.php",
			"/backend/empty.php/../../results/telemetry.php", "/frontend/../results/json.php",
			"/images/..%2fresults/telemetry.php", "/backend/garbage.php/.%2e/.%2e/results/",
		} {
			statusIs(t, s, "GET "+path, http.StatusNotFound, path)
		}
		stateIs(t, s.c, "state.json", `{"telemetry":false}`)
		isFile(t, s.c, "no telemetry_settings.php", "absent", generatedDir+"/telemetry_settings.php")
	})

	t.Run("switches-ignored", func(t *testing.T) {
		t.Parallel()
		s := serve(t, e, image, spec{port: portSwitchesIgnored, env: map[string]string{
			"REDACT_IP_ADDRESSES":   "true",
			"ENABLE_ID_OBFUSCATION": "true",
		}})
		logContains(t, s.c, "REDACT_IP_ADDRESSES and ENABLE_ID_OBFUSCATION without TELEMETRY: the log says they are ignored",
			"TELEMETRY is off, so these are ignored: REDACT_IP_ADDRESSES, ENABLE_ID_OBFUSCATION")
		statusIs(t, s, "GET /results/json.php with only the switches set", http.StatusNotFound, "/results/json.php")
		levelIs(t, s, "settings.json telemetry_level with only the switches set", "off")
	})

	t.Run("switch-checked", func(t *testing.T) {
		t.Parallel()
		// Ignored is not unchecked
		refuses(t, e, image, "ENABLE_ID_OBFUSCATION=1 without TELEMETRY", "ENABLE_ID_OBFUSCATION",
			refusal{env: map[string]string{"ENABLE_ID_OBFUSCATION": "1"}})
	})
}
