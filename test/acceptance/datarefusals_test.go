// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/divbast/ci/harness"
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
		harness.Refuses(t, e, refusing(image, telemetryOn()), refusedExit,
			"TELEMETRY=true with no volume, so on the image's read-only /data", needsWritableData)
	})

	t.Run("root-owned", func(t *testing.T) {
		t.Parallel()
		// Root's, as a host directory would be. A tmpfs behaves the same
		// under rootless Podman, where a host directory's owner is mapped.
		run := refusing(image, telemetryOn())
		run.Extra = []string{"--tmpfs", dataDir + ":mode=0755"}
		harness.Refuses(t, e, run, refusedExit, "TELEMETRY=true on a /data owned by root, mode 0755", needsWritableData)
	})

	// One volume, damaged a different way for each refusal in turn
	t.Run("unusable-files", func(t *testing.T) {
		t.Parallel()
		volume := newVolume(t, e)
		// onVolume is the refusal of a start with env, with the volume at /data
		onVolume := func(env map[string]string) *harness.RunSpec {
			run := refusing(image, env)
			run.Mounts = onData(volume)
			return run
		}
		// A database SQLite would open read-only
		phpOnVolume(t, e, image, volume, `touch("/data/telemetry.db"); chmod("/data/telemetry.db", 0444);`)
		harness.Refuses(t, e, onVolume(telemetryOn()), refusedExit, "TELEMETRY=true with a /data/telemetry.db uid 65532 cannot write",
			"/data/telemetry.db is there, but uid 65532 cannot read and write it")

		// An obfuscation salt PHP could not read, which would change every id
		// it hands out
		phpOnVolume(t, e, image, volume, `chmod("/data/telemetry.db", 0644);
			file_put_contents("/data/idObfuscation_salt.php", "<?php\n");
			chmod("/data/idObfuscation_salt.php", 0);`)
		harness.Refuses(t, e, onVolume(map[string]string{"TELEMETRY": "true", "ENABLE_ID_OBFUSCATION": "true"}), refusedExit,
			"ENABLE_ID_OBFUSCATION=true with a salt uid 65532 cannot read",
			"/data/idObfuscation_salt.php is there, but uid 65532 cannot read it")

		// A salt it can read, but not in the format upstream writes: empty
		// or cut short, as a full disk or a half-restored backup leaves it.
		// Upstream would read it as 0, or stop on it, and either changes
		// every id (LS-11).
		for _, salt := range []struct{ what, contents string }{
			{"an empty salt", ""},
			{"a salt cut short", "<?php\n\n$OBFUSCATION_SALT = 0x1f"},
			// Upstream's entrypoint writes as many hex digits as it is given,
			// and upstream uses the last 8 alone: more says more than the
			// salt is, so it is written down as those 8
			{"a salt of 9 hex digits", "<?php\n$OBFUSCATION_SALT = 0x1BA6C9260;\n"},
		} {
			phpOnVolume(t, e, image, volume, fmt.Sprintf(`file_put_contents("/data/idObfuscation_salt.php", base64_decode(%q));
				chmod("/data/idObfuscation_salt.php", 0600);`, base64.StdEncoding.EncodeToString([]byte(salt.contents))))
			harness.Refuses(t, e, onVolume(map[string]string{"TELEMETRY": "true", "ENABLE_ID_OBFUSCATION": "true"}), refusedExit,
				"ENABLE_ID_OBFUSCATION=true with "+salt.what, "/data/idObfuscation_salt.php is there, but is not a salt of 1 to 8 hex digits as upstream writes one")
		}

		// A database file SQLite does not recognise, as a disk fault or a
		// half-restored backup leaves it. Served, every result would be
		// answered with an empty 200 and recorded nowhere.
		phpOnVolume(t, e, image, volume, `file_put_contents("/data/telemetry.db", str_repeat("x", 4096));`)
		harness.Refuses(t, e, onVolume(telemetryOn()), refusedExit, "TELEMETRY=true with a /data/telemetry.db that is not a database",
			"cannot open /data/telemetry.db: SQLSTATE[HY000]: General error: 26 file is not a database")
	})
}
