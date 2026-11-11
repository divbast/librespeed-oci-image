// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package upstreamlayout tests test/upstream-layout.sh, the guard the
// Containerfile's fetch stage runs over its checkout of LibreSpeed. The
// guard runs against copies of testdata/ok, a stand-in for that checkout
// holding every path and string the image relies on, each changed the way
// a release might change it, and every way the guard can fail is seen
// failing.
package upstreamlayout

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// guard is the script under test, from this package's directory.
const guard = "../upstream-layout.sh"

// ok is the fixture every case starts from: a tree the guard passes.
const ok = "testdata/ok"

// edit rewrites one file of the tree. from is a regular expression that
// must match exactly once, so that a fixture which drifts fails the test
// rather than passing it for the wrong reason.
type edit struct {
	file, from, to string
}

// result is how a run of the guard ended.
type result struct {
	code           int
	stdout, stderr string
}

func TestUpstreamLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// remove deletes paths from the tree, and edits rewrite its files.
		remove []string
		edits  []edit
		// want holds one string per ERROR line the guard must print, each
		// naming the path at fault. Empty means it passes.
		want []string
	}{
		{name: "ok"},
		{
			name:   "missing-template",
			remove: []string{"settings.json"},
			want:   []string{"settings.json is missing"},
		},
		{
			name:   "missing-page-file",
			remove: []string{"stability_worker.js"},
			want:   []string{"stability_worker.js is missing"},
		},
		{
			name:   "missing-icon",
			remove: []string{"favicon.ico"},
			want:   []string{"favicon.ico is missing"},
		},
		{
			name:   "missing-licence",
			remove: []string{"LICENSE"},
			want:   []string{"LICENSE is missing"},
		},
		{
			name:   "missing-backend-file",
			remove: []string{"backend/getIP.php"},
			want:   []string{"backend/getIP.php is missing"},
		},
		{
			name:   "missing-results-file",
			remove: []string{"results/telemetry_db.php"},
			want:   []string{"results/telemetry_db.php is missing"},
		},
		{
			// A font the licences label names, inside a directory the
			// fetch stage copies whole
			name:   "missing-font",
			remove: []string{"frontend/fonts/Inter-latin-ext.woff2"},
			want:   []string{"frontend/fonts/Inter-latin-ext.woff2 is missing"},
		},
		{
			name:   "missing-directory",
			remove: []string{"images"},
			want:   []string{"images/ is missing"},
		},
		{
			// A file whose markers are checked is reported once, as
			// missing, and not again for each marker it no longer holds
			name:   "missing-marked-file",
			remove: []string{"index-modern.html"},
			want:   []string{"index-modern.html is missing"},
		},
		{
			name:  "marker-changed",
			edits: []edit{{"index-modern.html", `<title>LibreSpeed - `, "<title>LibreSpeed: "}},
			want:  []string{"index-modern.html no longer contains the marker: <title>LibreSpeed - Free"},
		},
		{
			// renderIndex refuses to start unless each marker it replaces
			// is there exactly once
			name:  "marker-twice",
			edits: []edit{{"index-modern.html", `(<h1>Free and Open Source Speedtest\.</h1>)`, "$1$1"}},
			want:  []string{"index-modern.html holds 2 copies of the marker: <h1>Free"},
		},
		{
			name:  "server-list-changed",
			edits: []edit{{"stability.html", `"server-list\.json"`, `"servers.json"`}},
			want:  []string{"stability.html no longer contains the marker: var SPEEDTEST_SERVERS"},
		},
		{
			name:  "login-check-changed",
			edits: []edit{{"results/stats.php", `=== \$stats_password`, "== $$stats_password"}},
			want:  []string{"results/stats.php no longer contains the marker: $_POST['password']"},
		},
		{
			name:  "salt-path-changed",
			edits: []edit{{"results/idObfuscation.php", `idObfuscation_salt\.php`, "salt.php"}},
			want:  []string{"results/idObfuscation.php no longer contains the marker: __DIR__"},
		},
		{
			name:  "apikey-set",
			edits: []edit{{"backend/getIP_ipInfo_apikey.php", `= '';`, "= '0123456789abcd';"}},
			want:  []string{"backend/getIP_ipInfo_apikey.php"},
		},
		{
			// A key from anywhere but a quoted literal, which no pattern
			// for a non-empty literal sees
			name:  "apikey-read-elsewhere",
			edits: []edit{{"backend/getIP_ipInfo_apikey.php", `(= '';\n)`, "$1$$IPINFO_APIKEY = getenv('IPINFO_APIKEY');\n"}},
			want:  []string{"backend/getIP_ipInfo_apikey.php"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tree := copyOK(t)
			for _, path := range tt.remove {
				remove(t, filepath.Join(tree, path))
			}
			for _, e := range tt.edits {
				rewrite(t, tree, e)
			}
			check(t, run(t, tree), tt.want)
		})
	}
}

// TestGuardCoversTheCopy fails when the Containerfile's fetch stage copies
// a path out of the checkout that the guard does not name: a release
// without it must fail the guard, naming it, before cp fails without
// saying why it mattered.
func TestGuardCoversTheCopy(t *testing.T) {
	t.Parallel()

	paths := copied(t)
	if len(paths) == 0 {
		t.Fatal("found no path the Containerfile copies from /src")
	}
	for _, pattern := range paths {
		matches, err := fs.Glob(os.DirFS(ok), pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Errorf("the Containerfile copies %s from the checkout, and %s holds nothing there to test the guard with", pattern, ok)
		}
		for _, path := range matches {
			t.Run(path, func(t *testing.T) {
				t.Parallel()

				tree := copyOK(t)
				remove(t, filepath.Join(tree, path))
				r := run(t, tree)
				if r.code != 1 {
					t.Fatalf("without %s: exit %d, want 1; stderr:\n%s", path, r.code, r.stderr)
				}
				if !slices.ContainsFunc(errorLines(r.stderr), func(line string) bool {
					return strings.Contains(line, path+" is missing") || strings.Contains(line, path+"/ is missing")
				}) {
					t.Errorf("without %s: no ERROR line says it is missing; stderr:\n%s", path, r.stderr)
				}
			})
		}
	}
}

// TestUsage checks the guard refuses a command line that names no tree,
// rather than reporting every path missing from a tree that is not there.
func TestUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "no-tree"},
		{name: "two-trees", args: []string{ok, ok}},
		{name: "not-a-directory", args: []string{filepath.Join(ok, "LICENSE")}},
		{name: "absent", args: []string{filepath.Join(ok, "absent")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := run(t, tt.args...)
			if r.code != 2 {
				t.Errorf("exit %d, want 2; stderr:\n%s", r.code, r.stderr)
			}
			if !strings.HasPrefix(r.stderr, "usage: ") || strings.Count(r.stderr, "\n") != 1 {
				t.Errorf("stderr is %q, want one usage line", r.stderr)
			}
		})
	}
}

// copied returns every path the Containerfile copies out of the checkout
// at /src, as it writes them: a glob stays a glob.
func copied(t *testing.T) []string {
	t.Helper()

	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	containerfile, err := root.ReadFile("Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`/src/([^\s"';]+)`).FindAllSubmatch(containerfile, -1)
	paths := make([]string, 0, len(found))
	for _, m := range found {
		paths = append(paths, string(m[1]))
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// copyOK copies the ok tree somewhere the test may change it.
func copyOK(t *testing.T) string {
	t.Helper()

	tree := t.TempDir()
	if err := os.CopyFS(tree, os.DirFS(ok)); err != nil {
		t.Fatal(err)
	}
	return tree
}

// rewrite applies e to its file in tree.
func rewrite(t *testing.T, tree string, e edit) {
	t.Helper()

	root, err := os.OpenRoot(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(e.from)
	if n := len(re.FindAllIndex(data, -1)); n != 1 {
		t.Fatalf("edit %q matches %s %d times, want once", e.from, e.file, n)
	}
	if err := root.WriteFile(e.file, re.ReplaceAll(data, []byte(e.to)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the fixture has nothing to remove: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// run runs the guard with args, and nothing of the host's environment but
// its PATH.
func run(t *testing.T, args ...string) result {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), guard, args...) //nolint:gosec // G204: the guard is this repository's own script, run on trees the test made
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Fatalf("%s: %v", guard, err)
		}
	}
	return result{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}

// check fails t unless r passed, saying so, when want is empty, or else
// failed with exactly one ERROR line for each string in want, containing
// it.
func check(t *testing.T, r result, want []string) {
	t.Helper()

	if len(want) == 0 {
		if r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "every file and marker the image relies on is in ") {
			t.Fatalf("exit %d, want 0 with the all-present line and nothing on stderr; stdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
		}
		return
	}
	if r.code != 1 {
		t.Fatalf("exit %d, want 1 with ERROR lines naming %q; stdout:\n%s\nstderr:\n%s", r.code, want, r.stdout, r.stderr)
	}
	lines := errorLines(r.stderr)
	if len(lines) != len(want) {
		t.Errorf("%d ERROR lines, want %d naming %q; stderr:\n%s", len(lines), len(want), want, r.stderr)
	}
	for _, w := range want {
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, w) }) {
			t.Errorf("no ERROR line contains %q; stderr:\n%s", w, r.stderr)
		}
	}
}

// errorLines returns the lines of stderr that report a failure.
func errorLines(stderr string) []string {
	var lines []string
	for line := range strings.Lines(stderr) {
		if strings.HasPrefix(line, "ERROR: ") {
			lines = append(lines, line)
		}
	}
	return lines
}
