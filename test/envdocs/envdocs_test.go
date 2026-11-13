// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

// Package envdocs holds the README's Env Vars table, the operator's whole
// list of settings, to the variables the image's PHP reads: a variable the
// table leaves out cannot be found, and one it names that nothing reads
// does nothing when set.
package envdocs

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The files the test reads, from the repository's root.
const (
	readmePath    = "README.md"
	bootstrapPath = "rootfs/usr/local/bin/librespeed-bootstrap"
	healthPath    = "rootfs/usr/local/bin/librespeed-health"
	loginPath     = "rootfs/usr/local/share/librespeed-login/login.php"
)

// sourcePaths is every PHP file the image runs. Only the bootstrap reads
// the environment today; a read added to another is a setting too.
var sourcePaths = []string{
	bootstrapPath,
	healthPath,
	"rootfs/usr/local/share/librespeed-health/index.php",
	loginPath,
}

// exemption is a read the README is not held to, allowed in one function of
// one file: one whose name is not written where it is read, or one of
// request data rather than a setting. function is "" for the file's top
// level.
type exemption struct {
	path, function, call string
}

// exemptions are the only reads the README is not held to, each by the
// scope it is written in, so the same call anywhere else fails. Reading
// $_SERVER is reading the environment too, but the names login.php reads
// are not settings: the server fills them in for each request.
var exemptions = []exemption{
	// The helpers every setting is read through. Their callers name the
	// variable, and are held to the README.
	{bootstrapPath, "envBool", "getenv($name)"},
	{bootstrapPath, "envString", "getenv($name)"},
	// The whole environment, which the server is exec'd with, less
	// STATS_PASSWORD
	{bootstrapPath, "", "getenv()"},
	// The address Caddy resolved for the request, and its method
	{loginPath, "client", "$_SERVER['LIBRESPEED_CLIENT_IP']"},
	{loginPath, "login", "$_SERVER['REQUEST_METHOD']"},
	// Where a browser says the request came from, and the host it was
	// sent to
	{loginPath, "fromAnotherOrigin", "$_SERVER['HTTP_SEC_FETCH_SITE']"},
	{loginPath, "fromAnotherOrigin", "$_SERVER['HTTP_ORIGIN']"},
	{loginPath, "fromAnotherOrigin", "$_SERVER['HTTP_HOST']"},
}

func TestEnvDocs(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	readme, err := root.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	sources := make([]source, 0, len(sourcePaths))
	for _, path := range sourcePaths {
		text, err := root.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source{path, text})
	}

	names, problems := audit(readme, sources, exemptions)
	for _, p := range problems {
		t.Error(p)
	}
	if len(names) == 0 {
		t.Fatal("audit found no variable the image reads")
	}
	if len(problems) == 0 {
		t.Logf("%s's Env Vars table names exactly the %d variables the image reads: %s",
			readmePath, len(names), strings.Join(names, " "))
	}
}

// The fixture every TestAudit case starts from: a bootstrap in miniature,
// with the helpers, two settings read through them, one over several
// lines, and the whole environment read for the server, and a README that
// documents those two settings.
const (
	okBootstrap = `#!/usr/bin/env php
<?php
// envString('IN_A_COMMENT') reads nothing

function envBool(string $name, bool $default): bool {
    $value = getenv($name);

    return $value === false || $value === '' ? $default : $value === 'true';
}

function envString(string $name): ?string {
    $value = getenv($name);

    return $value === false || $value === '' ? null : $value;
}

$telemetry = envBool('TELEMETRY', false);
$title = envString(
    'TITLE'
);
$serverEnv = getenv();
`
	okReadme = "# librespeed-oci-image\n\n" +
		"## Env Vars\n\n" +
		"| Variable | Default | Notes |\n" +
		"|---|---|---|\n" +
		"| `TELEMETRY` | `false` | Records results |\n" +
		"| `TITLE` | upstream's | The page's heading |\n\n" +
		"- An empty value counts as unset.\n\n" +
		"## Telemetry and stats\n\n" +
		"| `NOT_A_SETTING` | a table under another heading is not the Env Vars table |\n"
	okHealth = "#!/usr/bin/env php\n<?php\n\necho 'ok';\n"
	okLogin  = "<?php\n\nfunction client(): string {\n    return $_SERVER['LIBRESPEED_CLIENT_IP'];\n}\n\n" +
		"function login(): never {\n    $post = $_SERVER['REQUEST_METHOD'] === 'POST';\n}\n\n" +
		"function fromAnotherOrigin(): bool {\n    return [$_SERVER['HTTP_SEC_FETCH_SITE'], $_SERVER['HTTP_ORIGIN'], $_SERVER['HTTP_HOST']] !== [];\n}\n"
)

func TestAudit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// The fixture's files, changed: each replace edit applies once to
		// the bootstrap, the README or the health probe.
		bootstrap, readme, health, login []replace
		// want holds one string per problem audit must report, each
		// contained in it. Empty means it reports none.
		want []string
	}{
		{name: "ok"},
		{
			name:      "undocumented",
			bootstrap: []replace{{"$serverEnv", "$tagline = envString('TAGLINE');\n$serverEnv"}},
			want:      []string{"the image reads TAGLINE (" + bootstrapPath + ":21), and README.md's Env Vars table does not name it"},
		},
		{
			name:      "undocumented-from-env",
			bootstrap: []replace{{"$serverEnv", "$tagline = $_ENV['TAGLINE'];\n$serverEnv"}},
			want:      []string{"the image reads TAGLINE (" + bootstrapPath + ":21), and README.md's Env Vars table does not name it"},
		},
		{
			name:   "undocumented-from-server",
			health: []replace{{"echo 'ok';", "echo $_SERVER[\"HEALTH_TIMEOUT\"];"}},
			want:   []string{"the image reads HEALTH_TIMEOUT (" + healthPath + ":4), and README.md's Env Vars table does not name it"},
		},
		{
			name:      "hidden-name-in-env",
			bootstrap: []replace{{"$serverEnv", "$which = 'TITLE';\n$other = $_ENV[$which];\n$serverEnv"}},
			want:      []string{bootstrapPath + ":22: $_ENV[$which], at the top level, reads a variable whose name is not written there"},
		},
		{
			name:  "request-data-in-another-function",
			login: []replace{{"function login(): never {", "function other(): never {\n    $m = $_SERVER['REQUEST_METHOD'];\n}\n\nfunction login(): never {"}},
			want: []string{
				"the image reads REQUEST_METHOD (" + loginPath + ":8), and README.md's Env Vars table does not name it",
			},
		},
		{
			// The whole of $_SERVER, as a loop over it reads it, names no
			// variable, so only an exemption could allow it (LS-3)
			name:  "whole-superglobal",
			login: []replace{{"function login(): never {", "function headers(): array {\n    foreach ($_SERVER as $k => $v) {}\n}\n\nfunction login(): never {"}},
			want: []string{
				loginPath + ":8: $_SERVER, in headers(), reads a variable whose name is not written there",
			},
		},
		{
			name:  "request-data-exemption-unused",
			login: []replace{{"$_SERVER['REQUEST_METHOD'] === 'POST'", "true"}},
			want:  []string{"no read matches the exemption for $_SERVER['REQUEST_METHOD'], in login() of " + loginPath},
		},
		{
			name:   "unread",
			readme: []replace{{"| `TITLE` |", "| `GDPR_EMAIL` | none | Unread |\n| `TITLE` |"}},
			want:   []string{"README.md's Env Vars table names GDPR_EMAIL, which the image never reads"},
		},
		{
			name:   "duplicate-row",
			readme: []replace{{"| `TITLE` |", "| `TELEMETRY` | `true` | Again |\n| `TITLE` |"}},
			want:   []string{"README.md's Env Vars table has more than one row for TELEMETRY"},
		},
		{
			name:   "no-table",
			readme: []replace{{"## Env Vars", "## Settings"}},
			want:   []string{"README.md has no Env Vars table"},
		},
		{
			name:      "hidden-name",
			bootstrap: []replace{{"$serverEnv", "$which = 'TITLE';\n$other = envString($which);\n$serverEnv"}},
			want:      []string{bootstrapPath + ":22: envString($which), at the top level, reads a variable whose name is not written there"},
		},
		{
			name:      "concatenated-name",
			bootstrap: []replace{{"'TELEMETRY'", "'TELE' . 'METRY'"}},
			want: []string{
				bootstrapPath + ":17: envBool('TELE'.'METRY',false), at the top level, reads a variable",
				"README.md's Env Vars table names TELEMETRY, which the image never reads",
			},
		},
		{
			// Straight after envString's body, where a read is outside
			// every function but follows its declaration (#37)
			name:      "exemption-outside-its-function",
			bootstrap: []replace{{"    return $value === false || $value === '' ? null : $value;\n}\n", "    return $value === false || $value === '' ? null : $value;\n}\n$name = 'SECRET';\n$secret = getenv($name);\n"}},
			want:      []string{bootstrapPath + ":17: getenv($name), at the top level, reads a variable"},
		},
		{
			name:      "exemption-in-another-function",
			bootstrap: []replace{{"$telemetry", "function envInt(string $name): int {\n    return (int) getenv($name);\n}\n\n$telemetry"}},
			want:      []string{bootstrapPath + ":18: getenv($name), in envInt(), reads a variable"},
		},
		{
			name:      "exemption-in-a-closure",
			bootstrap: []replace{{"    $value = getenv($name);\n\n    return $value === false || $value === '' ? $default", "    $value = getenv($name);\n    $again = static function () use ($name) { return getenv($name); };\n\n    return $value === false || $value === '' ? $default"}},
			want:      []string{bootstrapPath + ":7: getenv($name), in {closure}(), reads a variable"},
		},
		{
			name:      "exemption-in-another-file",
			bootstrap: []replace{{"$serverEnv = getenv();\n", ""}},
			health:    []replace{{"echo 'ok';", "$env = getenv();"}},
			want: []string{
				healthPath + ":4: getenv(), at the top level, reads a variable",
				"no read matches the exemption for getenv(), at the top level of " + bootstrapPath,
			},
		},
		{
			name:   "read-in-another-file",
			health: []replace{{"echo 'ok';", "echo getenv('HEALTH_TIMEOUT');"}},
			want:   []string{"the image reads HEALTH_TIMEOUT (" + healthPath + ":4), and README.md's Env Vars table does not name it"},
		},
		{
			name:      "exemption-unused",
			bootstrap: []replace{{"    $value = getenv($name);\n\n    return $value === false || $value === '' ? $default", "    $value = getenv($name, true);\n\n    return $value === false || $value === '' ? $default"}},
			want: []string{
				bootstrapPath + ":6: getenv($name,true), in envBool(), reads a variable",
				"no read matches the exemption for getenv($name), in envBool() of " + bootstrapPath,
			},
		},
		{
			name:   "unparsable",
			health: []replace{{"echo 'ok';", "echo 'ok;"}},
			want:   []string{healthPath + ": cannot read its PHP"},
		},
		{
			name: "nothing-read",
			bootstrap: []replace{
				{"$telemetry = envBool('TELEMETRY', false);\n$title = envString(\n    'TITLE'\n);\n", ""},
			},
			want: []string{
				"found no variable the image reads, so there is nothing to hold README.md to",
				"README.md's Env Vars table names TELEMETRY, which the image never reads",
				"README.md's Env Vars table names TITLE, which the image never reads",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources := []source{
				{bootstrapPath, apply(t, okBootstrap, tt.bootstrap)},
				{healthPath, apply(t, okHealth, tt.health)},
				{loginPath, apply(t, okLogin, tt.login)},
			}
			names, problems := audit(apply(t, okReadme, tt.readme), sources, exemptions)
			if len(tt.want) == 0 && !slices.Equal(names, []string{"TELEMETRY", "TITLE"}) {
				t.Errorf("names = %q, want TELEMETRY and TITLE", names)
			}
			if len(problems) != len(tt.want) {
				t.Errorf("%d problems, want %d:\n%s", len(problems), len(tt.want), strings.Join(problems, "\n"))
			}
			for _, w := range tt.want {
				if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, w) }) {
					t.Errorf("no problem contains %q:\n%s", w, strings.Join(problems, "\n"))
				}
			}
		})
	}
}

// replace is one edit to a fixture: old, which must occur exactly once,
// becomes new.
type replace struct {
	old, new string
}

// apply makes edits to fixture, in order.
func apply(t *testing.T, fixture string, edits []replace) []byte {
	t.Helper()

	for _, e := range edits {
		if n := strings.Count(fixture, e.old); n != 1 {
			t.Fatalf("%q occurs %d times in the fixture, want once", e.old, n)
		}
		fixture = strings.Replace(fixture, e.old, e.new, 1)
	}
	return []byte(fixture)
}

// source is one PHP file the image runs.
type source struct {
	path string // from the repository's root, as problems name it
	text []byte
}

// row is a row of the Env Vars table whose first cell is a name in
// backticks; the name is that cell's.
var row = regexp.MustCompile("^\\| *`([A-Za-z_][A-Za-z0-9_]*)` *\\|")

// table returns the name in each row of readme's Env Vars table: every
// line under the heading "## Env Vars", up to the next heading of that
// level, outside fenced code, whose first cell is a name in backticks.
func table(readme []byte) []string {
	var (
		names         []string
		inTable, code bool
	)
	for line := range strings.Lines(string(readme)) {
		line = strings.TrimRight(line, " \t\r\n")
		switch {
		case strings.HasPrefix(line, "```"):
			code = !code
		case code:
		case strings.HasPrefix(line, "## "):
			inTable = line == "## Env Vars"
		case inTable:
			if m := row.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			}
		}
	}
	return names
}

// audit returns the names of the variables sources read, sorted, and
// every way readme's Env Vars table, or a read whose name is not written
// where it is read, falls short. None means the table names exactly what
// the image reads.
func audit(readme []byte, sources []source, exempt []exemption) (names, problems []string) {
	where := map[string]string{} // each name read, and where it is first read
	used := make([]bool, len(exempt))
	for _, s := range sources {
		found, err := reads(s.text)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: cannot read its PHP, so cannot say what it reads: %v", s.path, err))
			continue
		}
		for _, r := range found {
			i := slices.IndexFunc(exempt, func(e exemption) bool {
				return e.path == s.path && strings.EqualFold(e.function, r.function) && e.call == r.call
			})
			switch {
			case i >= 0:
				used[i] = true
				continue
			case r.name != "":
				if _, ok := where[r.name]; !ok {
					where[r.name] = fmt.Sprintf("%s:%d", s.path, r.line)
				}
				continue
			}
			problems = append(problems, fmt.Sprintf("%s:%d: %s, %s, reads a variable whose name is not written there,"+
				" so neither this test nor %s can name it", s.path, r.line, r.call, inScope(r.function), readmePath))
		}
	}
	for i, e := range exempt {
		if !used[i] {
			in := "in " + e.function + "() of "
			if e.function == "" {
				in = "at the top level of "
			}
			problems = append(problems, fmt.Sprintf("no read matches the exemption for %s, %s%s: remove it", e.call, in, e.path))
		}
	}
	names = slices.Sorted(maps.Keys(where))
	if len(names) == 0 {
		problems = append(problems, "found no variable the image reads, so there is nothing to hold "+readmePath+" to")
	}

	rows := table(readme)
	if len(rows) == 0 {
		// Every name read is then missing from it, which says no more
		problems = append(problems, readmePath+" has no Env Vars table: no row under \"## Env Vars\" starts with a `NAME` cell")
		slices.Sort(problems)
		return names, problems
	}
	count := map[string]int{}
	for _, name := range rows {
		count[name]++
	}
	documented := slices.Sorted(maps.Keys(count))
	for _, name := range documented {
		if count[name] > 1 {
			problems = append(problems, fmt.Sprintf("%s's Env Vars table has more than one row for %s", readmePath, name))
		}
		if _, ok := where[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s's Env Vars table names %s, which the image never reads", readmePath, name))
		}
	}
	for _, name := range names {
		if count[name] == 0 {
			problems = append(problems, fmt.Sprintf("the image reads %s (%s), and %s's Env Vars table does not name it",
				name, where[name], readmePath))
		}
	}
	slices.Sort(problems)
	return names, problems
}

// inScope says where a read is, for a problem: in which function, or at
// the top level.
func inScope(function string) string {
	if function == "" {
		return "at the top level"
	}
	return "in " + function + "()"
}
