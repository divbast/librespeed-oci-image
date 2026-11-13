// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/divbast/ci/harness"
)

// saltFormat is upstream's own format for the salt, the one
// idObfuscation.php writes itself when it finds none.
var saltFormat = regexp.MustCompile(`\A<\?php\n\n\$OBFUSCATION_SALT = 0x[0-9a-f]{8};\n\z`)

// obfuscatedID is what upstream's obfuscated ids look like: seven base-36
// digits.
var obfuscatedID = regexp.MustCompile(`\A[0-9a-z]{7}\z`)

// salt is what the probe's read-salt prints.
type salt struct {
	Contents string `json:"contents"`
	Mode     string `json:"mode"`
	// raw is the whole of what it printed, which a later read must match
	raw string
}

// readSalt runs the probe's read-salt in s.
func readSalt(t *testing.T, s *served) (salt, error) {
	t.Helper()
	out, err := output(t.Context(), s.probe, "read-salt")
	if err != nil {
		return salt{}, err
	}
	var got salt
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		return salt{}, fmt.Errorf("read-salt printed no JSON object: %w: %s", err, harness.Clip(out))
	}
	got.raw = out
	return got, nil
}

// TestSaltSurvives checks the obfuscation salt is made once, in upstream's
// format and private to uid 65532, and that it and the results it
// obfuscated survive a restart, and telemetry going off and on, so every
// share link handed out keeps working.
//
//nolint:paralleltest,tparallel // each phase restarts the server on the volume the one before it wrote, so they run in turn
func TestSaltSurvives(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	volume := newVolume(t, e)
	obfuscated := map[string]string{"TELEMETRY": "true", "ENABLE_ID_OBFUSCATION": "true"}

	// What the first start made: the salt, the stats_password upstream's
	// stats.php would compare a login with, and a result's id
	var (
		first    salt
		firstErr error
		password string
		id       string
	)
	// sameSalt checks the salt s reads is a salt, and the one first read.
	sameSalt := func(t *testing.T, s *served, what string) {
		t.Helper()
		checks := harness.NewChecks(t)
		what += ": the same salt"
		again, err := readSalt(t, s)
		switch {
		case firstErr != nil:
			checks.True(what, false, "the first start's salt could not be read: "+firstErr.Error())
		case err != nil:
			checks.True(what, false, err.Error())
		default:
			checks.True(what, saltFormat.MatchString(first.Contents) && again.raw == first.raw,
				fmt.Sprintf("first %s, now %s", harness.Clip(first.raw), harness.Clip(again.raw)))
		}
	}

	t.Run("first-start", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portSalt, volume: volume, env: obfuscated})
		checks := harness.NewChecks(t)
		first, firstErr = readSalt(t, s)
		if firstErr != nil {
			checks.True("the salt is in upstream's format", false, firstErr.Error())
			checks.True("the salt is private to uid 65532", false, firstErr.Error())
		} else {
			checks.True("the salt is in upstream's format", saltFormat.MatchString(first.Contents), harness.Clip(first.Contents))
			harness.Equal(checks, "the salt is private to uid 65532", "0600", first.Mode)
		}
		if generated, err := telemetrySettings(t.Context(), s.c); err == nil {
			password = statsPassword(generated)
		}

		id, _ = recordResult(t, s, "a result under ENABLE_ID_OBFUSCATION=true")
		// Unobfuscated, the first row in a new database is plainly 1
		checks.True("the id is obfuscated", obfuscatedID.MatchString(id) && id != "1", fmt.Sprintf("%q", id))
		resultIs(t, s, "GET /results/json.php?id="+id, id)
	})

	t.Run("restart", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portSalt, volume: volume, env: obfuscated})
		sameSalt(t, s, "after a restart")
		resultIs(t, s, "after a restart: GET /results/json.php?id="+id, id)
		again := ""
		if generated, err := telemetrySettings(t.Context(), s.c); err == nil {
			again = statsPassword(generated)
		}
		harness.NewChecks(t).True("after a restart: a new stats_password", password != "" && again != "" && again != password,
			fmt.Sprintf("%q, then %q", password, again))
	})

	t.Run("telemetry-off", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portSalt, volume: volume, env: map[string]string{"ENABLE_ID_OBFUSCATION": "true"}})
		statusIs(t, s, "with telemetry off: GET /results/json.php", http.StatusNotFound, "/results/json.php?id="+id)
		sameSalt(t, s, "with telemetry off")
	})

	t.Run("telemetry-on-again", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portSalt, volume: volume, env: obfuscated})
		sameSalt(t, s, "with telemetry on again")
		resultIs(t, s, "with telemetry on again: GET /results/json.php?id="+id, id)
	})
}
