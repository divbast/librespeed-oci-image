// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"net/http"
	"testing"
)

// TestRoutes checks the image serves only the modern UI, its generated
// config and the speed test, and answers 404 to everything else upstream
// ships.
func TestRoutes(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{port: portRoutes})

	tests := []struct {
		path string
		want int
	}{
		{path: "/", want: http.StatusOK},
		{path: "/settings.json", want: http.StatusOK},
		{path: "/server-list.json", want: http.StatusOK},
		{path: "/servers.json", want: http.StatusOK},
		{path: "/speedtest.js", want: http.StatusOK},
		{path: "/speedtest_worker.js", want: http.StatusOK},
		{path: "/stability.html", want: http.StatusOK},
		{path: "/stability_worker.js", want: http.StatusOK},
		{path: "/frontend/javascript/index.js", want: http.StatusOK},

		{path: "/index-classic.html", want: http.StatusNotFound},
		{path: "/config.json", want: http.StatusNotFound},
		{path: "/design-switch.js", want: http.StatusNotFound},
		{path: "/examples/example-singleServer-basic.html", want: http.StatusNotFound},
		{path: "/results/sanitycheck.php", want: http.StatusNotFound},
		{path: "/backend/getIP_util.php", want: http.StatusNotFound},
		{path: "/backend/getIP_ipInfo_apikey.php", want: http.StatusNotFound},
		{path: "/backend/geoip2.phar", want: http.StatusNotFound},
		{path: "/backend/country_asn.mmdb", want: http.StatusNotFound},
		{path: "/docker/entrypoint.sh", want: http.StatusNotFound},
		{path: "/frontend/index.html", want: http.StatusNotFound},
		{path: "/healthz", want: http.StatusNotFound},
		// Upstream's design switch, which picks a UI and so leads to the
		// classic one, and the modern UI under its own name: / serves it
		{path: "/index.html", want: http.StatusNotFound},
		{path: "/index-modern.html", want: http.StatusNotFound},
		// Caddy's path matcher takes another case for getIP.php, but no
		// file has that name; and a path past the script is no route
		{path: "/BACKEND/GETIP.PHP", want: http.StatusNotFound},
		{path: "/backend/getIP.php/x", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		what := "GET " + tt.path
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			statusIs(t, s, what, tt.want, tt.path)
		})
	}

	t.Run("modern-ui", func(t *testing.T) {
		t.Parallel()
		resp, err := s.get(t.Context(), "/", nil)
		if err != nil {
			t.Errorf("GET /: %v", err)
		}
		page := string(resp.body)
		contains(t, "GET / is the modern UI", `id="privacy-warning"`, page)
		lacks(t, "GET / does not lead to the classic UI", "index-classic", page)
		// Unset, TITLE, TAGLINE and GDPR_EMAIL leave upstream's own text
		contains(t, "GET / keeps upstream's heading", "<h1>Free and Open Source Speedtest.</h1>", page)
		contains(t, "GET / keeps upstream's tagline", `<p class="tagline">No Flash, No Java, No Websockets, No Bullsh*t</p>`, page)
	})
}
