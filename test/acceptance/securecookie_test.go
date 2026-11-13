// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// TestSecureCookie checks the session cookie a login hands out is Secure
// exactly when the login came over HTTPS, as far as the server can tell.
// It speaks plain HTTP itself, so behind a proxy that ends TLS it can tell
// only from that proxy's X-Forwarded-Proto, and only from a proxy
// TRUSTED_PROXIES names: any client can send the header (LS-15).
func TestSecureCookie(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	https := http.Header{"X-Forwarded-Proto": {"https"}}

	t.Run("trusted-proxy", func(t *testing.T) {
		t.Parallel()
		env := statsOn()
		// Every private range: the address this host's connections come
		// from on a network of the test's own is the gateway's on Docker,
		// and the container's own on rootless Podman, and either is in one
		env["TRUSTED_PROXIES"] = "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"
		s := serve(t, e, image, spec{volume: newVolume(t, e), network: newNetwork(t, e), env: env})
		secureIs(t, s, "from a trusted proxy, X-Forwarded-Proto: https", true, https)
		secureIs(t, s, "from a trusted proxy, X-Forwarded-Proto: http", false, http.Header{"X-Forwarded-Proto": {"http"}})
		secureIs(t, s, "from a trusted proxy, no X-Forwarded-Proto", false, nil)
	})

	t.Run("no-trusted-proxy", func(t *testing.T) {
		t.Parallel()
		s := serve(t, e, image, spec{volume: newVolume(t, e), network: newNetwork(t, e), env: statsOn()})
		secureIs(t, s, "TRUSTED_PROXIES unset, X-Forwarded-Proto: https", false, https)
	})
}

// secureIs logs in with the right password, from the host with header,
// and checks the login hands out a session cookie whose Secure flag is
// want.
func secureIs(t *testing.T, s *harness.Serving, what string, want bool, header http.Header) {
	t.Helper()
	checks := harness.NewChecks(t)
	what = fmt.Sprintf("%s: the login's %s cookie is Secure", what, sessionCookie)
	if !want {
		what = strings.Replace(what, "is Secure", "is not Secure", 1)
	}
	resp, err := postForm(t.Context(), s, harness.HostClient(), loginPath, header, url.Values{"password": {statsPasswordValue}})
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	var session *http.Cookie
	for _, line := range resp.Header.Values("Set-Cookie") {
		if c, err := http.ParseSetCookie(line); err == nil && c.Name == sessionCookie {
			session = c
		}
	}
	if resp.Status != http.StatusSeeOther || session == nil {
		checks.True(what, false, fmt.Sprintf("got %d, Set-Cookie: %q", resp.Status, resp.Header.Values("Set-Cookie")))
		return
	}
	checks.True(what, session.Secure == want, "Set-Cookie: "+session.String())
}
