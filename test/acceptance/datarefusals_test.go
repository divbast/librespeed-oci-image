// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"testing"
)

// The bootstrap's words for a /data that uid 65532 cannot write.
const needsWritableData = "needs a writable /data volume"

// TestDataRefusals checks TELEMETRY=true refuses a /data that uid 65532
// cannot write, and a database or salt the server could not use: each
// would be served as a speed test that records nothing, or hands out ids
// that change.
func TestDataRefusals(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)

	t.Run("no-volume", func(t *testing.T) {
		t.Parallel()
		refuses(t, e, image, "TELEMETRY=true with no volume, so on the image's read-only /data", needsWritableData,
			refusal{env: telemetryOn()})
	})

	t.Run("root-owned", func(t *testing.T) {
		t.Parallel()
		// Root's, as a host directory would be. A tmpfs behaves the same
		// under rootless Podman, where a host directory's owner is mapped.
		refuses(t, e, image, "TELEMETRY=true on a /data owned by root, mode 0755", needsWritableData,
			refusal{env: telemetryOn(), extra: []string{"--tmpfs", dataDir + ":mode=0755"}})
	})

	// One volume, damaged a different way for each refusal in turn
	t.Run("unusable-files", func(t *testing.T) {
		t.Parallel()
		volume := newVolume(t, e)
		// A database SQLite would open read-only
		phpOnVolume(t, e, image, volume, `touch("/data/telemetry.db"); chmod("/data/telemetry.db", 0444);`)
		refuses(t, e, image, "TELEMETRY=true with a /data/telemetry.db uid 65532 cannot write",
			"/data/telemetry.db is there, but uid 65532 cannot read and write it",
			refusal{env: telemetryOn(), volume: volume})

		// An obfuscation salt PHP could not read, which would change every id
		// it hands out
		phpOnVolume(t, e, image, volume, `chmod("/data/telemetry.db", 0644);
			file_put_contents("/data/idObfuscation_salt.php", "<?php\n");
			chmod("/data/idObfuscation_salt.php", 0);`)
		refuses(t, e, image, "ENABLE_ID_OBFUSCATION=true with a salt uid 65532 cannot read",
			"/data/idObfuscation_salt.php is there, but uid 65532 cannot read it",
			refusal{env: map[string]string{"TELEMETRY": "true", "ENABLE_ID_OBFUSCATION": "true"}, volume: volume})

		// A database file SQLite does not recognise, as a disk fault or a
		// half-restored backup leaves it. Served, every result would be
		// answered with an empty 200 and recorded nowhere.
		phpOnVolume(t, e, image, volume, `file_put_contents("/data/telemetry.db", str_repeat("x", 4096));`)
		refuses(t, e, image, "TELEMETRY=true with a /data/telemetry.db that is not a database",
			"cannot open /data/telemetry.db: SQLSTATE[HY000]: General error: 26 file is not a database",
			refusal{env: telemetryOn(), volume: volume})
	})
}
