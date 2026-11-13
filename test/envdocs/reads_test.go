// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package envdocs

import (
	"slices"
	"strings"
	"testing"
)

func TestReads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// src follows an opening <?php line, so its first line is line 2.
		src  string
		want []read
	}{
		{
			name: "literals",
			src:  "getenv('A');\nenvBool('B', true);\nenvString(\"C\");",
			want: []read{
				{line: 2, call: "getenv('A')", name: "A"},
				{line: 3, call: "envBool('B',true)", name: "B"},
				{line: 4, call: `envString("C")`, name: "C"},
			},
		},
		{
			name: "any-case-and-qualified",
			src:  "GETENV('A'); \\getenv('B'); EnvString('C');",
			want: []read{
				{line: 2, call: "GETENV('A')", name: "A"},
				{line: 2, call: "\\getenv('B')", name: "B"},
				{line: 2, call: "EnvString('C')", name: "C"},
			},
		},
		{
			name: "over-several-lines",
			src:  "$a = envString(\n    'A',\n);",
			want: []read{{line: 2, call: "envString('A',)", name: "A"}},
		},
		{
			name: "comments",
			src:  "// getenv('A')\n# envBool('B')\n/* envString('C')\n getenv('D') */",
		},
		{
			name: "strings",
			src:  "$a = 'getenv(\\'A\\')'; $b = \"envString('B')\"; $c = `getenv('C')`;",
		},
		{
			name: "heredoc-and-nowdoc",
			src:  "$a = <<<EOT\n  getenv('A') { }\n  EOT;\n$b = <<<'EOT'\ngetenv('B') {\nEOT;\n$c = <<<\"EOT\"\nenvString('C')\nEOT . getenv('D');",
			want: []read{{line: 10, call: "getenv('D')", name: "D"}},
		},
		{
			// An interpolation is code, so a call in it is a read
			name: "interpolations",
			src:  "$a = \"{$x[getenv('A')]}\";\n$b = <<<EOT\n{$y->z(envString('B'))}\nEOT;\n$c = \"${getenv('C')}\";\n$d = \"\\{$e[getenv('D')]}\";",
			want: []read{
				{line: 2, call: "getenv('A')", name: "A"},
				{line: 4, call: "envString('B')", name: "B"},
				{line: 6, call: "getenv('C')", name: "C"},
				{line: 7, call: "getenv('D')", name: "D"},
			},
		},
		{
			name: "simple-interpolation",
			src:  "$a = \"$b getenv('A') $c->d\";",
		},
		{
			name: "not-calls-of-the-readers",
			src:  "$o->getenv('A'); $o?->envBool('B'); C::envString('C'); new getenv('D'); \\Other\\getenv('E'); getenvx('F');",
		},
		{
			name: "declarations",
			src:  "class C {\n    public function getenv(string $name) {}\n    function &envString($name) {}\n}",
		},
		{
			name: "attributes-are-not-comments",
			src:  "#[Pure] function f() { return getenv('A'); }",
			want: []read{{line: 2, function: "f", call: "getenv('A')", name: "A"}},
		},
		{
			name: "outside-php",
			src:  "?>getenv('A') <?php getenv('B'); ?>\n<?= getenv('C') ?> // ?> getenv('D')\n<?php // getenv('E') ?> getenv('F')",
			want: []read{
				{line: 2, call: "getenv('B')", name: "B"},
				{line: 3, call: "getenv('C')", name: "C"},
			},
		},
		{
			name: "names-not-written-there",
			src:  "getenv($a); envString('A' . 'B'); envBool(\"$c\"); getenv(\"D\\x41\"); getenv(...); getenv(name: 'E'); getenv(); getenv(b'F');",
			want: []read{
				{line: 2, call: "getenv($a)"},
				{line: 2, call: "envString('A'.'B')"},
				{line: 2, call: `envBool("$c")`},
				{line: 2, call: `getenv("D\x41")`},
				{line: 2, call: "getenv(...)"},
				{line: 2, call: "getenv(name:'E')"},
				{line: 2, call: "getenv()"},
				{line: 2, call: "getenv(b'F')"},
			},
		},
		{
			// Each read is in the innermost function written around it,
			// a closure by PHP's own name for one, a method by its class's,
			// and an arrow function's in the function it is written in
			name: "scopes",
			src: `getenv($a);
function outer($name) {
    getenv($name);
    $f = function () use ($name) { return getenv($name); };
    $g = fn () => getenv($name);
    $h = match (true) { default => 1 };
    getenv($name);
}
getenv($name);
interface I { function m(); }
class C {
    public function envBool($name) { return getenv($name); }
}
$o = new class { function m($name) { return getenv($name); } };
enum E: string { case A = 'a'; }
getenv($name);`,
			want: []read{
				{line: 2, call: "getenv($a)"},
				{line: 4, function: "outer", call: "getenv($name)"},
				{line: 5, function: "{closure}", call: "getenv($name)"},
				{line: 6, function: "outer", call: "getenv($name)"},
				{line: 8, function: "outer", call: "getenv($name)"},
				{line: 10, call: "getenv($name)"},
				{line: 13, function: "C::envBool", call: "getenv($name)"},
				{line: 15, function: "class@anonymous::m", call: "getenv($name)"},
				{line: 17, call: "getenv($name)"},
			},
		},
		{
			// $_ENV and $_SERVER read the environment without a function
			// call; each is a read of the name its index spells out
			name: "superglobals",
			src: "$a = $_ENV['A'];\n$b = $_SERVER[\"B\"] ?? null;\n$c = $_ENV[$x];\n$d = $_SERVER['E' . 'F'];\n" +
				"function f() { return $_ENV [ 'G' ]; }\n$h = $_ENV[];",
			want: []read{
				{line: 2, call: "$_ENV['A']", name: "A"},
				{line: 3, call: `$_SERVER["B"]`, name: "B"},
				{line: 4, call: "$_ENV[$x]"},
				{line: 5, call: "$_SERVER['E'.'F']"},
				{line: 6, function: "f", call: "$_ENV['G']", name: "G"},
				{line: 7, call: "$_ENV[]"},
			},
		},
		{
			// A superglobal read whole, not indexed, reads every variable
			// at once, and so none the README could name (LS-3)
			name: "whole-superglobals",
			src: "foreach ($_SERVER as $k => $v) {}\n$e = $_ENV;\nfunction f() { return array_keys( $_ENV ); }\n" +
				"$g = isset($_SERVER) ? 1 : 0;",
			want: []read{
				{line: 2, call: "$_SERVER"},
				{line: 3, call: "$_ENV"},
				{line: 4, function: "f", call: "$_ENV"},
				{line: 5, call: "$_SERVER"},
			},
		},
		{
			name: "not-reads-of-the-superglobals",
			src:  "$a = $env['A']; $b = $o->_ENV['B']; $c = C::$_ENV['C']; $d = $_ENVX['D']; $e = $_env['E']; $f = C::$_SERVER;\n$g = '$_ENV[\\'H\\']';",
		},
		{
			// An anonymous class is a scope however it is declared, and a
			// read in it belongs to its method
			name: "anonymous-classes",
			src: "$a = new readonly class { function m($n) { return getenv($n); } };\n" +
				"$b = new readonly class extends B { function m($n) { return getenv($n); } };\n" +
				"$d = new class(1) extends B { function m($n) { return getenv($n); } };\n" +
				"getenv($n);",
			want: []read{
				{line: 2, function: "class@anonymous::m", call: "getenv($n)"},
				{line: 3, function: "class@anonymous::m", call: "getenv($n)"},
				{line: 4, function: "class@anonymous::m", call: "getenv($n)"},
				{line: 5, call: "getenv($n)"},
			},
		},
		{
			name: "braces-in-strings-keep-their-scope",
			src:  "function f() {\n    $a = '}';\n    $b = \"{\";\n    $c = <<<'X'\n}\nX;\n    getenv($name);\n}\ngetenv($name);",
			want: []read{
				{line: 8, function: "f", call: "getenv($name)"},
				{line: 10, call: "getenv($name)"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := reads([]byte("<?php\n" + tt.src))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("reads =\n%s\nwant\n%s", show(got), show(tt.want))
			}
		})
	}
}

// TestReadsRefuses checks the scanner fails on what it cannot read to the
// end, rather than skipping the rest of a file.
func TestReadsRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, src, want string
	}{
		{name: "string", src: "<?php\necho 'a;", want: "line 2: a string that never ends"},
		{name: "double-quoted", src: "<?php\necho \"a;", want: "line 2: a string that never ends"},
		{name: "comment", src: "<?php\n/* a", want: "line 2: a comment that never ends"},
		{name: "heredoc", src: "<?php\n$a = <<<EOT\nb\n", want: "line 2: a heredoc that never ends"},
		{name: "heredoc-label", src: "<?php\n$a = <<< ;", want: "line 2: <<< opens no heredoc"},
		{name: "interpolation", src: "<?php\n$a = \"{$b", want: "line 2: an interpolation that never ends"},
		{name: "call", src: "<?php\ngetenv('A'", want: "line 2: a call that never ends"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := reads([]byte(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// show lists reads one per line, for a failure.
func show(rs []read) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString("  ")
		b.WriteString(r.String())
		b.WriteByte('\n')
	}
	return b.String()
}
