// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"net/http"
	"testing"

	"github.com/divbast/ci/harness"
)

// TestServesWithNoVolume runs the image under the exact contract with no
// volume and nothing set, as a user who only runs it would, and checks it
// serves the speed test to the host.
func TestServesWithNoVolume(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)

	s, err := e.StartServing(t.Context(), t, spec{}.serveSpec(image))
	if err != nil {
		t.Fatal(err)
	}
	detail := "GET /backend/getIP.php answered"
	if err = s.Wait(t.Context(), "/backend/getIP.php"); err != nil {
		detail = err.Error()
	}
	harness.NewChecks(t).True("container serves", err == nil, detail)
	harness.StatusIs(t, s, "/backend/getIP.php", http.StatusOK, "/backend/getIP.php")
}
