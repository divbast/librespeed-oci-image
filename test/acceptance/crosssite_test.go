// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/divbast/ci/harness"
)

// crossSite is what login.php says when it turns away a login a browser
// says came from another site.
const crossSite = "This login was sent from another site"

// TestCrossSiteLogin checks a login a browser says came from another
// origin is refused before it is counted, right password or wrong, so no
// page elsewhere can have its visitors' browsers post wrong passwords and
// lock their own addresses out. Sec-Fetch-Site says so when a browser
// sends it, which it does only to HTTPS and localhost addresses; without
// it, as over plain HTTP, an Origin that names another host says so. A
// login from the page's own origin goes on, and so does one that carries
// neither header, from a client that is no browser (LS-14).
func TestCrossSiteLogin(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{volume: newVolume(t, e), env: statsOn()})
	b := newBrowser(t, s)

	// One failure that counts, from the page's own origin, so the count
	// is there to stay as it is
	sameOrigin := http.Header{"Sec-Fetch-Site": {"same-origin"}}
	b.loginIs(t, "Sec-Fetch-Site: same-origin, a wrong password", http.StatusForbidden, "wrong", sameOrigin)
	counted, err := failureBuckets(t.Context(), s.C)
	if err != nil {
		counted = err.Error()
	}
	harness.NewChecks(t).True("that wrong password is counted", err == nil && oneFailure.MatchString(counted), counted)

	for _, tt := range []struct {
		what   string
		header http.Header
	}{
		{"Sec-Fetch-Site: cross-site", http.Header{"Sec-Fetch-Site": {"cross-site"}}},
		{"Sec-Fetch-Site: same-site", http.Header{"Sec-Fetch-Site": {"same-site"}}},
		{"Sec-Fetch-Site: cross-site, with this host's own Origin", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {s.Base}}},
		{"no Sec-Fetch-Site, an Origin on another site", http.Header{"Origin": {"https://elsewhere.example"}}},
		{"no Sec-Fetch-Site, an Origin on this host at another port", http.Header{"Origin": {otherPort(t, s.Base)}}},
		{"no Sec-Fetch-Site, Origin: null", http.Header{"Origin": {"null"}}},
	} {
		for _, password := range []string{"wrong", statsPasswordValue} {
			what := fmt.Sprintf("%s, password %q", tt.what, password)
			got := b.loginIs(t, what+": refused", http.StatusForbidden, password, tt.header)
			harness.Contains(t, what+": says why", crossSite, got.body)
		}
	}
	bucketsAre(t, s.C, "no login refused as from another site is counted", counted)
	if page, err := b.page(t.Context()); err != nil {
		t.Errorf("GET %s: %v", statsPath, err)
	} else {
		harness.Lacks(t, "no login refused as from another site logs in", loggedIn, page)
	}

	// Sec-Fetch-Site decides when a browser sends it: an Origin that names
	// another host, as behind a proxy that rewrites Host, changes nothing
	b.loginIs(t, "Sec-Fetch-Site: same-origin, with an Origin on another host: the right password", http.StatusSeeOther,
		statsPasswordValue, http.Header{"Sec-Fetch-Site": {"same-origin"}, "Origin": {"https://speedtest.example"}})
	b.loginIs(t, "Sec-Fetch-Site: none, as for an address typed in: the right password", http.StatusSeeOther,
		statsPasswordValue, http.Header{"Sec-Fetch-Site": {"none"}})
	b.loginIs(t, "no Sec-Fetch-Site, with this host's own Origin: the right password", http.StatusSeeOther,
		statsPasswordValue, http.Header{"Origin": {s.Base}})
	b.loginIs(t, "neither header, as curl sends it: the right password", http.StatusSeeOther, statsPasswordValue, nil)
}

// otherPort returns base, an http:// origin, at the next port, or fails t.
func otherPort(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("%s names no port: %v", base, err)
	}
	return "http://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port%65535+1))
}
