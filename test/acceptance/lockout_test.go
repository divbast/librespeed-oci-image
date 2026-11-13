// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// Where login.php records failed logins, and the lock it holds while it
// checks a password.
const (
	statsLoginDB   = dataDir + "/stats-login.db"
	statsLoginLock = generatedDir + "/stats-login.lock"
)

// loginPath is where upstream's form posts a login.
const loginPath = statsPath + "?op=login"

// loopbackLogin is the login as a client inside the server's container
// sends it, from 127.0.0.1.
const loopbackLogin = "http://127.0.0.1:8080" + loginPath

// What login.php says when it turns a login away, busy or for a client
// at its limit.
const (
	busy      = "Another login is being checked"
	overLimit = "Too many failed logins"
)

// failureLimit is how many failures inside 900 s lock a client out.
const failureLimit = 5

// The network every container of TestLockout runs on, and its address
// there, so the server sees one client across a restart.
const (
	lockoutSubnet = "10.89.66.0/24"
	lockoutIP     = "10.89.66.200"
)

// failureLimits is one client's five failed logins, the limit.
var failureLimits = regexp.MustCompile(`\A([0-9a-f.:/]+)=5\z`)

// digits is a number of seconds, as Retry-After gives one.
var digits = regexp.MustCompile(`\A\d+\z`)

// statsOn is the environment that turns telemetry and the stats page on.
func statsOn() map[string]string {
	return map[string]string{"TELEMETRY": "true", "STATS_PASSWORD": statsPasswordValue}
}

// TestLockout checks five failures lock a client out for 900 s, keyed on
// the connection's own address, which no header changes; that a 429
// records nothing; that a restart forgives no failure, kept on /data;
// and that one password is checked at a time, server-wide, without a
// client at its limit ever holding that lock against another.
//
//nolint:paralleltest,tparallel // each phase restarts the server on the volume the one before it wrote, so they run in turn
func TestLockout(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	sp := spec{port: portLockout, volume: newVolume(t, e), network: newNetwork(t, e, lockoutSubnet), ip: lockoutIP, env: statsOn()}

	t.Run("five-failures", func(t *testing.T) {
		s := serve(t, e, image, sp)
		checks := harness.NewChecks(t)
		ctx := t.Context()

		if global, err := generated(ctx, s.c, "caddy/global.caddy"); err != nil {
			checks.True("TRUSTED_PROXIES unset: global.caddy trusts no proxy", false, err.Error())
		} else {
			harness.Lacks(t, "TRUSTED_PROXIES unset: global.caddy trusts no proxy", "trusted_proxies", global)
		}
		clientIPOptionsAre(t, s.c, "TRUSTED_PROXIES unset: the server trusts no proxy",
			`[{"trusted_proxies":null,"trusted_proxies_strict":null,"client_ip_headers":null}]`)

		// Every header a client might hope the key comes from:
		// X-Forwarded-For, the server variable's own name, and the header
		// PHP would map onto that name
		b := newBrowser(t, s)
		for i := 1; i <= failureLimit; i++ {
			b.loginIs(t, fmt.Sprintf("wrong password %d of 5, as 203.0.113.%d and 198.51.100.%d", i, i, i),
				http.StatusForbidden, fmt.Sprintf("wrong-%d", i), claimed(i))
		}
		header := claimed(6)
		header.Set("X-Real-IP", "198.51.100.7")
		header.Set("Client-Ip", "198.51.100.8")
		got := b.loginIs(t, "the right password after five failures, under new addresses in every header",
			http.StatusTooManyRequests, statsPasswordValue, header)
		wait, err := strconv.Atoi(got.retryAfter)
		checks.True("429 says when to try again", digits.MatchString(got.retryAfter) && err == nil && wait >= 1 && wait <= 900,
			"Retry-After: "+got.retryAfter)

		buckets, err := failureBuckets(ctx, s.c)
		if err != nil {
			buckets = err.Error()
		}
		m := failureLimits.FindStringSubmatch(buckets)
		checks.True("one client, keyed on the connection's own address, with five failures",
			err == nil && m != nil && !strings.HasPrefix(m[1], "203.0.113.") && !strings.HasPrefix(m[1], "198.51.100."), buckets)
		b.loginIs(t, "a sixth wrong password", http.StatusTooManyRequests, "wrong-6", nil)
		again, err := failureBuckets(ctx, s.c)
		checks.True("a 429 records nothing", err == nil && m != nil && again == buckets,
			fmt.Sprintf("%q before, %q after", buckets, again))
	})

	t.Run("restart", func(t *testing.T) {
		// The failures are on /data, so a restart forgives none of them
		s := serve(t, e, image, sp)
		b := newBrowser(t, s)
		b.loginIs(t, "after a restart: the right password", http.StatusTooManyRequests, statsPasswordValue, nil)
		harness.RunRecords(t, s.probe, "age-failures", "901")
		b.loginIs(t, "after age-failures 901: the right password", http.StatusSeeOther, statsPasswordValue, nil)

		// A second client, apart from this host: the server's own loopback
		loopbackIs(t, s.probe, "from the server's own loopback, a second client: a wrong password",
			http.StatusForbidden, "http-request", "wrong-1")
		bucketsAre(t, s.c, "the loopback's failure is counted apart from this host", "127.0.0.1=1")

		// One password is checked at a time, server-wide. While the probe
		// holds login.php's lock, even the right password is turned away at
		// once, and nothing is recorded.
		body := loopbackIs(t, s.probe, "the right password while the probe holds the lock",
			http.StatusTooManyRequests, "http-request-locked", statsPasswordValue)
		harness.Contains(t, "the 429 says why", busy, body)
		bucketsAre(t, s.c, "turned away while busy, nothing is recorded", "127.0.0.1=1")

		betweenThreads(t, s, b)

		// A client already at its limit is answered before the lock is
		// tried, so no flood from it ever holds the lock against anyone else
		for i := 2; i <= failureLimit; i++ {
			loopbackIs(t, s.probe, fmt.Sprintf("from the loopback: wrong password %d of 5", i),
				http.StatusForbidden, "http-request", fmt.Sprintf("wrong-%d", i+1))
		}
		bucketsAre(t, s.c, "the loopback is at its limit", "127.0.0.1=5")
		body = loopbackIs(t, s.probe, "at its limit, while the probe holds the lock: the right password",
			http.StatusTooManyRequests, "http-request-locked", statsPasswordValue)
		harness.Contains(t, "at its limit, it is told so, not that a check is under way", overLimit, body)

		duringFlood(t, s, b)
	})
}

// claimed is the header of a client that claims the address 203.0.113.i
// in X-Forwarded-For, and 198.51.100.i in the server variable login.php
// reads and the header PHP would map onto it. Go writes each name in its
// canonical case; a server reads a name whatever its case, as Go's own
// does, so the server sees exactly what curl sent it.
func claimed(i int) http.Header {
	h := http.Header{}
	h.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
	h.Set("LIBRESPEED_CLIENT_IP", fmt.Sprintf("198.51.100.%d", i))
	h.Set("Librespeed-Client-Ip", fmt.Sprintf("198.51.100.%d", i))
	return h
}

// betweenThreads checks two logins sent to two of the server's own
// threads are checked one at a time, as one sent by the probe while a
// login is checked is. With a hash at bcrypt cost 16 a check takes
// seconds, so login A is caught holding the lock in /proc/locks, read
// rather than probed with a lock of the test's own, and login B, sent
// then, is turned away.
func betweenThreads(t *testing.T, s *served, b *browser) {
	t.Helper()
	ctx := t.Context()
	checks := harness.NewChecks(t)
	harness.RunRecords(t, s.probe, "write-stats-hash", statsPasswordValue, "16")

	type answered struct {
		status int
		err    error
	}
	loginA := make(chan answered, 1)
	go func() {
		client := harness.HostClient()
		client.Timeout = 2 * time.Minute
		got, err := (&browser{s: s, client: client}).login(ctx, loginPath, statsPasswordValue, nil)
		loginA <- answered{status: got.status, err: err}
	}()

	// Until login A holds the lock, or is answered first
	var (
		a       *answered
		holders []holder
		err     error
	)
	deadline := time.Now().Add(30 * time.Second)
	for a == nil && len(holders) == 0 && err == nil && time.Now().Before(deadline) {
		select {
		case got := <-loginA:
			a = &got
		default:
			holders, err = lockHolders(ctx, s.probe, statsLoginLock)
		}
	}
	const holds = "login A holds the lock: an flock(2) write lock, held by the process running the server's argv"
	switch {
	case err != nil:
		checks.True(holds, false, err.Error())
	case len(holders) == 0:
		checks.True(holds, false, "no lock on "+statsLoginLock+" was seen before login A was answered or 30 s passed")
	default:
		// Found by the PID that holds it, never taken to be PID 1: the
		// holder is whatever process runs the server's argv
		h := holders[0]
		checks.True(holds, len(holders) == 1 && h.kind == "FLOCK" && h.access == "WRITE" && slices.Equal(h.argv, serverArgv),
			fmt.Sprintf("%v; the server is %q", holders, serverArgv))
	}

	body := loopbackIs(t, s.probe, "login B, sent meanwhile to another of the server's threads",
		http.StatusTooManyRequests, "http-request", "wrong-2")
	harness.Contains(t, "login B: the 429 says why", busy, body)
	if a == nil {
		got := <-loginA
		a = &got
	}
	answers(t, "login A, once checked: the right password", http.StatusSeeOther, response{status: a.status}, a.err)
	bucketsAre(t, s.c, "login B recorded nothing", "127.0.0.1=1")

	// PHP 8.5's default cost, as the bootstrap writes it
	harness.RunRecords(t, s.probe, "write-stats-hash", statsPasswordValue, "12")
	b.loginIs(t, "with the hash back at cost 12, the lock free: the right password", http.StatusSeeOther, statsPasswordValue, nil)
}

// floodSize is how many logins the flood sends at once.
const floodSize = 4

// duringFlood checks a flood from a client at its limit, sent while the
// database's write lock is held, never takes login.php's lock: any of it
// that did would sit inside it until the hold ends, and this host's
// login, sent meanwhile, would be turned away.
func duringFlood(t *testing.T, s *served, b *browser) {
	t.Helper()
	ctx := t.Context()
	checks := harness.NewChecks(t)
	type held struct {
		out string
		err error
	}
	hold := make(chan held, 1)
	go func() {
		out, err := output(ctx, s.probe, "hold-db-lock", "3")
		hold <- held{out: out, err: err}
	}()
	// Until SQLite's write lock on the database is held, as /proc/locks
	// shows it: a POSIX write lock, taken by hold-db-lock
	locked, err := pollHolders(ctx, s.probe, statsLoginDB, 10*time.Second, func(h holder) bool {
		return h.kind == "POSIX" && h.access == "WRITE" && slices.Contains(h.argv, "hold-db-lock")
	})

	var (
		flood    [floodSize]harness.HTTPResponse
		floodErr [floodSize]error
		answered atomic.Int32
		wg       sync.WaitGroup
	)
	for i := range floodSize {
		wg.Go(func() {
			flood[i], floodErr[i] = loopbackRequest(ctx, s.probe, "http-request", statsPasswordValue)
			answered.Add(1)
		})
	}
	// Until the flood is answered, or one of it holds the lock
	deadline := time.Now().Add(10 * time.Second)
	for answered.Load() < floodSize && time.Now().Before(deadline) {
		if holders, herr := lockHolders(ctx, s.probe, statsLoginLock); herr != nil || len(holders) > 0 {
			break
		}
	}
	b.loginIs(t, "this host, during a flood from the loopback: the right password", http.StatusSeeOther, statsPasswordValue, nil)
	wg.Wait()

	refused := 0
	for i := range floodSize {
		if floodErr[i] == nil && flood[i].Status == http.StatusTooManyRequests && strings.Contains(string(flood[i].Body), overLimit) {
			refused++
		} else {
			t.Logf("flood %d: %d %v: %s", i+1, flood[i].Status, floodErr[i], harness.Clip(string(flood[i].Body)))
		}
	}
	harness.Equal(checks, "the flood: each told it is over its limit", floodSize, refused)
	bucketsAre(t, s.c, "the flood recorded nothing", "127.0.0.1=5")

	h := <-hold
	switch {
	case err != nil:
		checks.True("the database write lock is held", false, err.Error())
	case h.err != nil:
		checks.True("the database write lock is held", false, h.err.Error())
	default:
		checks.True("the database write lock is held", len(locked) > 0 && h.out == "held\nreleased\n",
			fmt.Sprintf("%v, and hold-db-lock printed %q", locked, h.out))
	}
}

// holder is one lock on a file, as the probe's lock-holders reads it from
// /proc/locks: the kind, FLOCK or POSIX, the access, READ or WRITE, the
// holder's PID, and its argv, nil if that could not be read.
type holder struct {
	kind, access, pid string
	argv              []string
}

// String says who holds the lock, for a check's detail.
func (h holder) String() string {
	return fmt.Sprintf("%s %s by PID %s %q", h.kind, h.access, h.pid, h.argv)
}

// lockHolders returns every lock held on path in p's container. The probe
// only reads: probing with a lock of its own could take the very lock it
// looks for.
func lockHolders(ctx context.Context, p *harness.Probe, path string) ([]holder, error) {
	out, err := output(ctx, p, "lock-holders", path)
	if err != nil {
		return nil, err
	}
	var holders []holder
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimSuffix(line, "\n"), " ", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("lock-holders printed what is not a lock: %q", line)
		}
		h := holder{kind: f[0], access: f[1], pid: f[2]}
		if err := json.Unmarshal([]byte(f[3]), &h.argv); err != nil {
			return nil, fmt.Errorf("lock-holders printed an argv that is not JSON: %w: %q", err, line)
		}
		holders = append(holders, h)
	}
	return holders, nil
}

// pollHolders asks for the locks on path until one that match says is
// held, and returns those, or none once within has passed.
func pollHolders(ctx context.Context, p *harness.Probe, path string, within time.Duration, match func(holder) bool) ([]holder, error) {
	deadline := time.Now().Add(within)
	for {
		holders, err := lockHolders(ctx, p, path)
		if err != nil {
			return nil, err
		}
		if found := slices.DeleteFunc(holders, func(h holder) bool { return !match(h) }); len(found) > 0 {
			return found, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		if err := harness.Pause(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// loopbackRequest posts password to the login from inside p's container,
// so from 127.0.0.1, a client apart from the host. sub is the probe's
// http-request, or http-request-locked to send it while the probe holds
// login.php's lock.
func loopbackRequest(ctx context.Context, p *harness.Probe, sub, password string) (harness.HTTPResponse, error) {
	out, err := output(ctx, p, sub, http.MethodPost, loopbackLogin, "", "password="+password)
	if err != nil {
		return harness.HTTPResponse{}, err
	}
	status, body, _ := strings.Cut(out, "\n")
	code, err := strconv.Atoi(status)
	if err != nil || len(status) != 3 {
		return harness.HTTPResponse{}, fmt.Errorf("%s printed no status line: %s", sub, harness.Clip(out))
	}
	return harness.HTTPResponse{Status: code, Body: []byte(body)}, nil
}

// loopbackIs sends the login as loopbackRequest does, checks its status
// is want, and returns its body.
func loopbackIs(t *testing.T, p *harness.Probe, what string, want int, sub, password string) string {
	t.Helper()
	got, err := loopbackRequest(t.Context(), p, sub, password)
	answers(t, what, want, response{status: got.Status}, err)
	return string(got.Body)
}

// clientIPOptionsAre checks the client-address options of every server,
// as the running server adapts its own Caddyfile, are want: each distinct
// set once, in a fixed order, as jq's unique leaves them.
func clientIPOptionsAre(t *testing.T, c *harness.Container, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	got, err := clientIPOptions(t.Context(), c)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, got)
}

// clientIPOptions returns, as JSON, the client-address options of every
// server in c's own Caddyfile, as the server adapts it.
func clientIPOptions(ctx context.Context, c *harness.Container) (string, error) {
	res, err := c.Exec(ctx, nil, harness.FrankenPHP, "adapt", "--config", "/etc/frankenphp/Caddyfile")
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("could not adapt the server's Caddyfile: exit status %d: %s", res.Code, res.Stderr)
	}
	var adapted struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					TrustedProxies       json.RawMessage `json:"trusted_proxies"`
					TrustedProxiesStrict json.RawMessage `json:"trusted_proxies_strict"`
					ClientIPHeaders      json.RawMessage `json:"client_ip_headers"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(res.Stdout, &adapted); err != nil {
		return "", fmt.Errorf("the adapted Caddyfile is not JSON: %w", err)
	}
	options := make([]string, 0, len(adapted.Apps.HTTP.Servers))
	for _, srv := range adapted.Apps.HTTP.Servers {
		// A field the server leaves out is null, as jq reads it
		raw := func(m json.RawMessage) json.RawMessage {
			if len(m) == 0 {
				return json.RawMessage("null")
			}
			return m
		}
		o, err := encode(struct {
			TrustedProxies       json.RawMessage `json:"trusted_proxies"`
			TrustedProxiesStrict json.RawMessage `json:"trusted_proxies_strict"`
			ClientIPHeaders      json.RawMessage `json:"client_ip_headers"`
		}{raw(srv.TrustedProxies), raw(srv.TrustedProxiesStrict), raw(srv.ClientIPHeaders)})
		if err != nil {
			return "", err
		}
		options = append(options, o)
	}
	slices.Sort(options)
	return "[" + strings.Join(slices.Compact(options), ",") + "]", nil
}
