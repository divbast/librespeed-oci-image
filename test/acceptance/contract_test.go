// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/divbast/ci/contract"
	"github.com/divbast/ci/harness"
	"github.com/divbast/librespeed-oci-image/test/probe"
)

// libreSpeed is LibreSpeed's contract, as its Containerfile and README
// state it, in the configuration a user who sets nothing gets: telemetry
// off. ready, when set, is the contract's ReadyProbe.
func libreSpeed(image string, ready func(context.Context, *harness.Container, *harness.Probe) error) *contract.Config {
	return &contract.Config{
		Image:      image,
		AppPort:    appPort,
		HealthPort: healthPort,
		TmpfsSize:  tmpfsSize,
		PID1:       serverArgv,
		HealthCmd:  healthCmd,
		Healthcheck: contract.Healthcheck{
			Test:        slices.Concat([]string{"CMD"}, healthCmd),
			Interval:    30 * time.Second,
			Timeout:     10 * time.Second,
			StartPeriod: 60 * time.Second,
			Retries:     3,
		},
		ReadyProbe:    ready,
		ProbeApp:      probe.App,
		HealthOnly404: []string{"/", "/backend/getIP.php", "/index.php"},
		Scenario:      "telemetry off",
	}
}

// TestContract runs divbast's confinement contract against the image, on a
// network with no route out: no capability, a read-only root, uid 65532,
// the server as PID 1, no shell, two listeners, no egress, the HEALTHCHECK
// and the health port. It also checks what only LibreSpeed can say of
// that network: the speed test answers there, as it does anywhere, so
// egress refused there is the refusal of a server that works.
func TestContract(t *testing.T) {
	t.Parallel()
	_, image := underTest(t)

	// The contract installs the probe, with LibreSpeed's subcommands,
	// before it asks whether the app is ready, and hands it over (LS-5)
	var getIP string
	getIPErr := errors.New("the container under the contract never came up, so getIP.php was never asked")
	contract.Run(t, libreSpeed(image, func(ctx context.Context, _ *harness.Container, p *harness.Probe) error {
		var err error
		getIP, err = loopbackGet(ctx, p, "/backend/getIP.php")
		getIPErr = err
		return err
	}))

	checks := harness.NewChecks(t)
	const what = "serves GET /backend/getIP.php inside the internal network"
	if getIPErr != nil {
		checks.True(what, false, getIPErr.Error())
		return
	}
	harness.Equal(checks, what, getIPLoopback, getIP)
}
