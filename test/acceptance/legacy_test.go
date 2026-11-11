// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package acceptance runs the acceptance suite against the image IMAGE
// names, under the engine ENGINE names, docker unless it says podman.
package acceptance

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLegacySuite runs the bash acceptance suite, test/acceptance.sh,
// against IMAGE under ENGINE, while its sections move to Go. Its output
// goes to the test log, and the test fails when the suite does.
//
//nolint:paralleltest // the suite owns fixed container names and host ports, so it runs alone
func TestLegacySuite(t *testing.T) {
	image := os.Getenv("IMAGE")
	if image == "" {
		t.Skip("IMAGE is unset, so there is no image to test: just acceptance sets it, and ENGINE, to the image just build wrote")
	}
	engine := os.Getenv("ENGINE")
	if engine == "" {
		engine = "docker"
	}
	t.Logf("test/acceptance.sh against %s under %s", image, engine)

	// The suite reads IMAGE and ENGINE from the environment it inherits,
	// and changes into its own directory before it reads anything else.
	cmd := exec.CommandContext(t.Context(), "../acceptance.sh")
	cmd.Stdout = t.Output()
	cmd.Stderr = t.Output()
	// A process the suite left running in the background could hold its
	// output open after the suite exits. Wait that long for it, then stop.
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Run(); err != nil {
		t.Fatalf("test/acceptance.sh: %v", err)
	}
}
