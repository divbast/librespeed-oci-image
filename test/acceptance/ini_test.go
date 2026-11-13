// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// phpINI is what the probe's php-ini prints. A field the JSON lacks stays
// nil, so it can never pass for false or zero.
type phpINI struct {
	AllowURLFopen    *bool    `json:"allow_url_fopen"`
	DisplayErrors    *bool    `json:"display_errors"`
	LogErrors        *bool    `json:"log_errors"`
	ErrorLog         *string  `json:"error_log"`
	ErrorReporting   []string `json:"error_reporting"`
	PostMaxSizeBytes *int64   `json:"post_max_size_bytes"`
}

// phpInMemory is how much of a body PHP keeps in memory. It writes the
// rest to /tmp.
const phpInMemory = 16384

// The names of the checks of php-ini's settings, in the order they run.
const (
	checkAllowURLFopen  = "allow_url_fopen"
	checkDisplayErrors  = "display_errors"
	checkLogErrors      = "log_errors"
	checkErrorLog       = "error_log is unset"
	checkErrorReporting = "error_reporting"
	checkPostMaxSize    = "post_max_size is under PHP's 16 KiB in-memory buffer"
)

// value formats a field php-ini printed, or says it printed none.
func value[T any](v *T) string {
	if v == nil {
		return "missing"
	}
	return fmt.Sprint(*v)
}

// TestPHPSettings checks PHP refuses URL streams, logs real errors to
// stderr only, and keeps request bodies off /tmp: a body PHP wrote there
// would take the 16 MiB tmpfs's room.
func TestPHPSettings(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{})
	checks := harness.NewChecks(t)

	if ini, err := readINI(t, s); err != nil {
		for _, what := range []string{
			checkAllowURLFopen, checkDisplayErrors, checkLogErrors, checkErrorLog, checkErrorReporting, checkPostMaxSize,
		} {
			checks.True(what, false, err.Error())
		}
	} else {
		harness.Equal(checks, checkAllowURLFopen, "false", value(ini.AllowURLFopen))
		harness.Equal(checks, checkDisplayErrors, "false", value(ini.DisplayErrors))
		harness.Equal(checks, checkLogErrors, "true", value(ini.LogErrors))
		// Unset, so PHP hands each line to FrankenPHP's logger: uid 65532
		// cannot open /dev/stderr, and a file would keep errors out of the
		// container log
		harness.Equal(checks, checkErrorLog, "", value(ini.ErrorLog))
		// Real errors and uncaught exceptions, which are E_ERROR, and nothing
		// a client could raise by the thousand: no warning, notice or
		// deprecation
		harness.Equal(checks, checkErrorReporting, "E_ERROR E_PARSE E_CORE_ERROR E_COMPILE_ERROR E_USER_ERROR E_RECOVERABLE_ERROR",
			strings.Join(ini.ErrorReporting, " "))
		// PHP reads no body bigger than post_max_size, unless that is 0,
		// which lifts the limit altogether
		size := ini.PostMaxSizeBytes
		checks.True(checkPostMaxSize, size != nil && *size > 0 && *size < phpInMemory,
			fmt.Sprintf("%s bytes, where 1 to %d would do", value(size), phpInMemory-1))
	}

	// FrankenPHP logs PHP's errors as the php handler. The server's own
	// config, as the server adapts it, sends that logger to stderr and
	// keeps it out of the default log on stdout, where the speed test's
	// access log goes too: the upload checks read the bytes the server
	// took from it, as an operator reading stdout would.
	const logging = "PHP's error log goes to stderr, and the rest of the server's log to stdout"
	if got, err := serverLogging(t, s); err != nil {
		checks.True(logging, false, err.Error())
	} else {
		harness.Equal(checks, logging, `{"php":["stderr"],"default":"stdout","default_excludes_php":true,"access":["stdout"]}`, got)
	}

	const unnamed = "responses do not name PHP and its version"
	if resp, err := s.Get(t.Context(), "/backend/getIP.php", nil); err != nil {
		checks.True(unnamed, false, err.Error())
	} else {
		powered := resp.Header.Values("X-Powered-By")
		checks.True(unnamed, len(powered) == 0, fmt.Sprintf("X-Powered-By: %q", powered))
	}
}

// readINI runs the probe's php-ini in s.
func readINI(t *testing.T, s *harness.Serving) (phpINI, error) {
	t.Helper()
	res, err := s.Probe.Output(t.Context(), "php-ini")
	if err != nil {
		return phpINI{}, err
	}
	if res.Code != 0 {
		return phpINI{}, fmt.Errorf("php-ini: exit status %d: %s%s", res.Code, res.Stdout, res.Stderr)
	}
	var ini phpINI
	if err := json.Unmarshal(res.Stdout, &ini); err != nil {
		return phpINI{}, fmt.Errorf("php-ini printed no JSON object: %w: %s", err, harness.Clip(string(res.Stdout)))
	}
	return ini, nil
}

// serverLogging returns, as JSON, where the running server's own config
// sends PHP's log and the default log, whether the default leaves PHP's
// out, and where the speed test's own access log goes.
func serverLogging(t *testing.T, s *harness.Serving) (string, error) {
	t.Helper()
	res, err := s.C.Exec(t.Context(), nil, harness.FrankenPHP, "adapt", "--config", "/etc/frankenphp/Caddyfile")
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("could not adapt the server's Caddyfile: exit status %d: %s", res.Code, res.Stderr)
	}
	type logger struct {
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
		Writer  struct {
			Output string `json:"output"`
		} `json:"writer"`
	}
	var adapted struct {
		Logging struct {
			Logs map[string]logger `json:"logs"`
		} `json:"logging"`
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
					Logs   *struct {
						DefaultLoggerName string `json:"default_logger_name"`
					} `json:"logs"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err = json.Unmarshal(res.Stdout, &adapted); err != nil {
		return "", fmt.Errorf("the adapted Caddyfile is not JSON: %w", err)
	}

	const php = "http.handlers.php"
	logs := adapted.Logging.Logs
	var where struct {
		PHP                []string `json:"php"`
		Default            string   `json:"default"`
		DefaultExcludesPHP bool     `json:"default_excludes_php"`
		Access             []string `json:"access"`
	}
	// A site's log block names a logger, which takes that site's access
	// log alone
	var access []string
	for _, srv := range adapted.Apps.HTTP.Servers {
		if slices.Contains(srv.Listen, fmt.Sprintf(":%d", appPort)) && srv.Logs != nil {
			access = append(access, "http.log.access."+srv.Logs.DefaultLoggerName)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(logs)) {
		if slices.Contains(logs[name].Include, php) {
			where.PHP = append(where.PHP, logs[name].Writer.Output)
		}
		if slices.ContainsFunc(access, func(a string) bool { return slices.Contains(logs[name].Include, a) }) {
			where.Access = append(where.Access, logs[name].Writer.Output)
		}
	}
	where.Default = logs["default"].Writer.Output
	where.DefaultExcludesPHP = slices.Contains(logs["default"].Exclude, php)
	b, err := json.Marshal(where)
	return string(b), err
}
