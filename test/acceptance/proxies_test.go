// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
	"github.com/divbast/librespeed-oci-image/test/probe"
)

// The network a server and its proxy run on, which the server trusts, and
// their addresses there.
const (
	proxySubnet = "10.89.77.0/24"
	proxiedIP   = "10.89.77.10"
	proxyIP     = "10.89.77.20"
)

// TestTrustedProxies checks a server behind a proxy TRUSTED_PROXIES names
// keys its login limit on the right-most address in X-Forwarded-For that
// it does not trust, an IPv4 address alone, an IPv4-mapped one as that
// address, and IPv6 by its /64. Only addresses and CIDR ranges reach the
// Caddyfile, never one that takes in every address, and each as PHP
// writes it.
func TestTrustedProxies(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)

	t.Run("behind-a-proxy", func(t *testing.T) {
		t.Parallel()
		behindProxy(t, e, image)
	})

	for _, tt := range []struct{ what, value, holds string }{
		{"TRUSTED_PROXIES=not-a-cidr", "not-a-cidr", "'not-a-cidr', which is not an IP address or CIDR range"},
		{"TRUSTED_PROXIES with a /33", "10.0.0.0/8 10.0.0.0/33", "'10.0.0.0/33', which is not an IP address or CIDR range"},
		{"TRUSTED_PROXIES that would close the servers block", "10.0.0.0/8 }\nadmin 0.0.0.0:2019", "'}', which is not an IP address or CIDR range"},
		{"TRUSTED_PROXIES with Caddy's own private_ranges", "private_ranges", "'private_ranges', which is not an IP address or CIDR range"},
		// A prefix of 0 takes in every address of its family, so every
		// client of it is a trusted proxy. One that writes an address of
		// the other family in X-Forwarded-For, which no such range holds,
		// is counted as that address: a new bucket for every guess.
		{"TRUSTED_PROXIES=0.0.0.0/0", "0.0.0.0/0", "'0.0.0.0/0', which takes in every address"},
		{"TRUSTED_PROXIES=::/0", "::/0", "'::/0', which takes in every address"},
		{"TRUSTED_PROXIES with 10.0.0.0/0 among other ranges", "192.0.2.1 10.0.0.0/0", "'10.0.0.0/0', which takes in every address"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			t.Parallel()
			harness.Refuses(t, e, refusing(image, map[string]string{"TRUSTED_PROXIES": tt.value}), refusedExit,
				tt.what, "TRUSTED_PROXIES holds "+tt.holds)
		})
	}

	t.Run("as-php-writes-it", func(t *testing.T) {
		t.Parallel()
		// What global.caddy holds is each address as PHP writes it, never
		// the variable's own text
		s := serve(t, e, image, spec{port: portProxyList, env: map[string]string{
			"TRUSTED_PROXIES": " 10.89.77.0/24,,2001:DB8::/32  192.0.2.1 ",
		}})
		const what = "a list with commas, spaces and capitals, as written to global.caddy"
		checks := harness.NewChecks(t)
		global, err := generated(t.Context(), s.c, "caddy/global.caddy")
		if err != nil {
			checks.True(what, false, err.Error())
			return
		}
		var lines []string
		for line := range strings.Lines(global) {
			if strings.Contains(line, "trusted_proxies static") {
				lines = append(lines, strings.TrimSuffix(line, "\n"))
			}
		}
		harness.Equal(checks, what, "\ttrusted_proxies static 10.89.77.0/24 2001:db8::/32 192.0.2.1", strings.Join(lines, "\n"))
	})
}

// behindProxy runs the server, and a second container of the same image in
// front of it as its proxy, so the suite needs no other image, on a
// network whose addresses the server trusts. Every login goes from the
// proxy to the server, with the X-Forwarded-For a proxy would send.
func behindProxy(t *testing.T, e harness.Engine, image string) {
	t.Helper()
	ctx := t.Context()
	checks := harness.NewChecks(t)
	network := newNetwork(t, e, proxySubnet)
	env := statsOn()
	env["TRUSTED_PROXIES"] = proxySubnet
	server, err := e.Start(ctx, t, &harness.RunSpec{
		Image:   image,
		Network: network,
		IP:      proxiedIP,
		Confine: harness.Contract(tmpfsSize),
		Env:     env,
		Mounts:  onData(newVolume(t, e)),
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := e.Start(ctx, t, &harness.RunSpec{
		Image:   image,
		Network: network,
		IP:      proxyIP,
		Confine: harness.Contract(tmpfsSize),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := harness.InstallProbe(ctx, proxy, probe.App)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://%s:%d", proxiedIP, appPort)
	if err := servesProxy(t, server, p, base); err != nil {
		t.Fatal(err)
	}

	if global, err := generated(ctx, server, "caddy/global.caddy"); err != nil {
		checks.True("global.caddy trusts "+proxySubnet, false, err.Error())
	} else {
		harness.Contains(t, "global.caddy trusts "+proxySubnet, "trusted_proxies static "+proxySubnet, global)
	}
	clientIPOptionsAre(t, server, "the server trusts X-Forwarded-For from the proxy, read right to left",
		fmt.Sprintf(`[{"trusted_proxies":{"ranges":[%q],"source":"static"},"trusted_proxies_strict":1,"client_ip_headers":["X-Forwarded-For"]}]`, proxySubnet))

	// login sends one login through the proxy, for the client
	// X-Forwarded-For names, and checks the status is want.
	login := func(what string, want int, forwarded, password string) {
		t.Helper()
		resp, err := p.HTTP(ctx, http.MethodPost, base+loginPath, []string{"X-Forwarded-For: " + forwarded}, "password="+password)
		answers(t, what, want, response{status: resp.Status}, err)
	}
	for i := 1; i <= failureLimit; i++ {
		login(fmt.Sprintf("client A, 203.0.113.7: wrong password %d of 5", i), http.StatusForbidden, "203.0.113.7", fmt.Sprintf("wrong-%d", i))
	}
	login("client A: the right password after five failures", http.StatusTooManyRequests, "203.0.113.7", statsPasswordValue)
	// IPv4 counts per address, and an IPv4-mapped IPv6 address as its IPv4
	login("203.0.113.8, beside client A: the right password", http.StatusSeeOther, "203.0.113.8", statsPasswordValue)
	login("client A's address written IPv4-mapped, ::ffff:203.0.113.7: the right password", http.StatusTooManyRequests,
		"::ffff:203.0.113.7", statsPasswordValue)
	// The proxy appends the address it saw. Whatever the client wrote
	// before it is the client's own claim.
	login("client A's address claimed in front of 198.51.100.9: the right password", http.StatusSeeOther,
		"203.0.113.7, 198.51.100.9", statsPasswordValue)
	// IPv6 counts per /64, what one subscriber is usually given
	for i := 1; i <= failureLimit; i++ {
		login(fmt.Sprintf("client B, 2001:db8::1: wrong password %d of 5", i), http.StatusForbidden, "2001:db8::1", fmt.Sprintf("wrong-%d", i))
	}
	login("2001:db8::2, in client B's /64: the right password", http.StatusTooManyRequests, "2001:db8::2", statsPasswordValue)
	login("2001:db8:0:1::1, in the next /64: the right password", http.StatusSeeOther, "2001:db8:0:1::1", statsPasswordValue)
	bucketsAre(t, server, "the failures are client A's and client B's /64's alone", "2001:db8::/64=5\n203.0.113.7=5")
}

// servesProxy asks for GET / at base from the proxy once a second until it
// answers 200. A server that stops first, or one still not serving after
// serveWithin, is an error carrying its log.
func servesProxy(t *testing.T, server *harness.Container, p *harness.Probe, base string) error {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(serveWithin)
	for {
		resp, err := p.HTTP(ctx, http.MethodGet, base+"/", nil, "")
		if err == nil && resp.Status == http.StatusOK {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("GET %s/ from the proxy answered %d", base, resp.Status)
		}
		ins, ierr := server.Inspect(ctx)
		switch {
		case ierr != nil:
			return fmt.Errorf("%w; and then: %w", err, ierr)
		case !ins.State.Running:
			return harness.WithLog(ctx, server, fmt.Errorf("container %s stopped, exit status %d, before it served its proxy", server, ins.State.ExitCode))
		case !time.Now().Before(deadline):
			return harness.WithLog(ctx, server, fmt.Errorf("container %s never served its proxy within %s: %w", server, serveWithin, err))
		}
		if err := harness.Pause(ctx, time.Second); err != nil {
			return err
		}
	}
}
