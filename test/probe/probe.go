// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package probe holds LibreSpeed's own subcommands for divbast's
// in-container probe, for the acceptance suite to install.
package probe

import (
	_ "embed" // for App
)

// App is app.php: PHP that returns LibreSpeed's subcommands. The harness's
// InstallProbe puts it beside its core probe, which adds them to its own,
// and the contract suite takes it as Config.ProbeApp.
//
//go:embed app.php
var App []byte
