// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"testing"

	"github.com/divbast/ci/harness"
)

// TestHostileValuesRenderEscaped sets TITLE, TAGLINE and GDPR_EMAIL to
// markup, and checks the UI shows each as text, never as markup, in place
// of upstream's own.
func TestHostileValuesRenderEscaped(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{env: map[string]string{
		"TITLE":      `Tést <b>&'"`,
		"TAGLINE":    "<script>x</script>",
		"GDPR_EMAIL": "a@b.example",
	}})

	page := string(fetch(t, s, "/"))
	// htmlspecialchars(..., ENT_QUOTES | ENT_HTML5, 'UTF-8')
	const title = "Tést &lt;b&gt;&amp;&apos;&quot;"
	for _, tt := range []struct{ what, needle string }{
		{what: "TITLE in <title>, escaped", needle: "<title>" + title + " - Free and Open Source Speedtest</title>"},
		{what: "TITLE in <h1>, escaped", needle: "<h1>" + title + "</h1>"},
		{what: "TAGLINE, escaped", needle: `<p class="tagline">&lt;script&gt;x&lt;/script&gt;</p>`},
		{what: "GDPR_EMAIL in the mailto: link", needle: `href="mailto:a@b.example"`},
		{what: "GDPR_EMAIL in place of upstream's placeholder text", needle: ">a@b.example</a>"},
	} {
		harness.Contains(t, tt.what, tt.needle, page)
	}
	for _, tt := range []struct{ what, needle string }{
		{what: "no raw <b>", needle: "<b>"},
		{what: "no raw <script>x", needle: "<script>x"},
		{what: "no upstream placeholder address", needle: "PUT@YOUR_EMAIL.HERE"},
	} {
		harness.Lacks(t, tt.what, tt.needle, page)
	}
}
