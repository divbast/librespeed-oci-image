// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// The login form upstream's stats page shows, which posts into the
// image's login, and what only a logged-in page shows.
const (
	loginForm = `<form action="stats.php?op=login" method="POST">`
	loggedIn  = `value="Logout"`
)

// sessionCookie is the cookie PHP's session lives in.
const sessionCookie = "PHPSESSID"

// oneFailure is one failed login, recorded against one client.
var oneFailure = regexp.MustCompile(`\A[0-9a-f.:/]+=1\z`)

// browser is a client from the host with a cookie jar of its own, as one
// browser would be.
type browser struct {
	s      *served
	client *http.Client
	jar    http.CookieJar
}

// newBrowser returns a browser for s with an empty cookie jar.
func newBrowser(t *testing.T, s *served) *browser {
	t.Helper()
	// cookiejar.New fails only for options it is not given
	jar, _ := cookiejar.New(nil)
	client := harness.HostClient()
	client.Jar = jar
	return &browser{s: s, client: client, jar: jar}
}

// loginAnswer is what a password posted to a stats URL got back.
type loginAnswer struct {
	status               int
	location, retryAfter string
	// sent is the bytes of the body sent
	sent int
	body string
}

// login posts password to path, as upstream's login form does, with
// header.
func (b *browser) login(ctx context.Context, path, password string, header http.Header) (loginAnswer, error) {
	form := url.Values{"password": {password}}
	resp, err := b.s.postForm(ctx, b.client, path, header, form)
	if err != nil {
		return loginAnswer{}, err
	}
	return loginAnswer{
		status:     resp.status,
		location:   resp.header.Get("Location"),
		retryAfter: resp.header.Get("Retry-After"),
		sent:       len(form.Encode()),
		body:       string(resp.body),
	}, nil
}

// loginIs logs in as [browser.login] does, as upstream's form posts it,
// and checks the status is want.
func (b *browser) loginIs(t *testing.T, what string, want int, password string, header http.Header) loginAnswer {
	t.Helper()
	got, err := b.login(t.Context(), loginPath, password, header)
	answers(t, what, want, response{status: got.status}, err)
	return got
}

// page returns the stats page, as this browser sees it.
func (b *browser) page(ctx context.Context) (string, error) {
	resp, err := b.s.do(ctx, b.client, http.MethodGet, statsPath, nil, nil)
	return string(resp.body), err
}

// session returns the PHP session id the browser's jar holds, or "".
func (b *browser) session() string {
	u, err := url.Parse(b.s.base + "/")
	if err != nil {
		return ""
	}
	for _, c := range b.jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

// failureBuckets returns login.php's failures still inside its window, as
// client=count lines, by client. It opens the database read-only, so it
// never creates one.
func failureBuckets(ctx context.Context, c *harness.Container) (string, error) {
	out, err := phpIn(ctx, c, `echo json_encode((new PDO("sqlite:/data/stats-login.db", null, null,
			[PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY]))
		->query("SELECT client, COUNT(*) FROM failures WHERE at > " . (time() - 900) . " GROUP BY client ORDER BY client")
		->fetchAll(PDO::FETCH_KEY_PAIR), JSON_FORCE_OBJECT), "\n";`)
	if err != nil {
		return "", err
	}
	var counts map[string]json.Number
	if err := json.Unmarshal([]byte(out), &counts); err != nil {
		return "", fmt.Errorf("the failures are not a JSON object: %w: %s", err, clip(out))
	}
	lines := make([]string, 0, len(counts))
	for _, client := range slices.Sorted(maps.Keys(counts)) {
		lines = append(lines, client+"="+counts[client].String())
	}
	return strings.Join(lines, "\n"), nil
}

// bucketsAre checks failureBuckets is want.
func bucketsAre(t *testing.T, c *harness.Container, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	got, err := failureBuckets(t.Context(), c)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, got)
}

// TestStatsLogin sets STATS_PASSWORD with telemetry on, and checks
// upstream's own form posts into the image's login: the password is
// stored only as a bcrypt hash and never reaches the server's environment
// or log, a wrong one is counted, the right one logs in with a new session
// id, and no session id planted beforehand is logged in. Upstream's own
// check, against a stats_password random on every start, decides nothing
// however a login is written, and a path that leaves stats.php never runs
// it. Restarted with a new password, the old one no longer opens it.
//
//nolint:paralleltest,tparallel // each phase restarts the server on the volume the one before it wrote, so they run in turn
func TestStatsLogin(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	volume := newVolume(t, e)

	t.Run("set", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portStatsLogin, volume: volume, env: map[string]string{
			"TELEMETRY":      "true",
			"STATS_PASSWORD": statsPasswordValue,
		}})
		checks := harness.NewChecks(t)
		ctx := t.Context()

		const stored = "/data/stats-password.hash"
		if hash, err := phpIn(ctx, s.c, `const HASH = "/data/stats-password.hash";
			echo json_encode(["mode" => sprintf("%04o", (int) @fileperms(HASH) & 0o7777),
				"algo" => password_get_info((string) @file_get_contents(HASH))["algoName"],
				"verifies" => password_verify("`+statsPasswordValue+`", (string) @file_get_contents(HASH)),
				"plain" => str_contains((string) @file_get_contents(HASH), "`+statsPasswordValue+`")]), "\n";`); err != nil {
			checks.True(stored, false, err.Error())
		} else {
			harness.Equal(checks, stored, `{"mode":"0600","algo":"bcrypt","verifies":true,"plain":false}`, strings.TrimSuffix(hash, "\n"))
		}
		logLacks(t, s.c, "the log never shows STATS_PASSWORD", statsPasswordValue)
		// The server starts without it, so no PHP the server runs can read
		// it back. TELEMETRY shows the environment was read at all.
		const environment = "the server's environment"
		if environ, err := phpIn(ctx, s.c, `const ENVIRON = "/proc/1/environ";
			echo json_encode(["read" => @file_get_contents(ENVIRON) !== false,
				"TELEMETRY" => str_contains((string) @file_get_contents(ENVIRON), "TELEMETRY=true"),
				"STATS_PASSWORD" => str_contains((string) @file_get_contents(ENVIRON), "STATS_PASSWORD=")]), "\n";`); err != nil {
			checks.True(environment, false, err.Error())
		} else {
			harness.Equal(checks, environment, `{"read":true,"TELEMETRY":true,"STATS_PASSWORD":false}`, strings.TrimSuffix(environ, "\n"))
		}

		statusIs(t, s, "GET /results/stats.php", http.StatusOK, statsPath)
		b := newBrowser(t, s)
		page, err := b.page(ctx)
		if err != nil {
			t.Errorf("GET %s: %v", statsPath, err)
		}
		contains(t, "GET /results/stats.php is upstream's login form", loginForm, page)
		before := b.session()
		wrong := b.loginIs(t, "POST /results/stats.php?op=login with a wrong password", http.StatusForbidden, "wrong", nil)
		contains(t, "a wrong password: the form again", loginForm, wrong.body)
		recorded, err := failureBuckets(ctx, s.c)
		if err != nil {
			recorded = err.Error()
		}
		checks.True("the wrong password is recorded", err == nil && oneFailure.MatchString(recorded), recorded)
		const fits = "the login fits post_max_size"
		if ini, ierr := readINI(t, s); ierr != nil {
			checks.True(fits, false, ierr.Error())
		} else {
			limit := ini.PostMaxSizeBytes
			checks.True(fits, limit != nil && wrong.sent > 0 && int64(wrong.sent) < *limit,
				fmt.Sprintf("%d of %s bytes", wrong.sent, value(limit)))
		}

		const right = "POST /results/stats.php?op=login with the right password"
		if got, lerr := b.login(ctx, loginPath, statsPasswordValue, nil); lerr != nil {
			checks.True(right, false, lerr.Error())
		} else {
			harness.Equal(checks, right, "303 "+statsPath, fmt.Sprintf("%d %s", got.status, got.location))
		}
		after := b.session()
		checks.True("the login hands out a new session id", before != "" && after != "" && before != after,
			fmt.Sprintf("%q became %q", before, after))
		page, err = b.page(ctx)
		if err != nil {
			t.Errorf("GET %s: %v", statsPath, err)
		}
		contains(t, "following it with the cookie, GET /results/stats.php is logged in", loggedIn, page)
		// Empty proves nothing unless the failure was there to clear
		const cleared = "the login clears that client's failures"
		if left, ferr := failureBuckets(ctx, s.c); ferr != nil {
			checks.True(cleared, false, ferr.Error())
		} else {
			checks.True(cleared, oneFailure.MatchString(recorded) && left == "",
				fmt.Sprintf("%q before the login, %q after it", recorded, left))
		}

		// A session id an attacker planted before the login is not the one
		// logged in
		const fixation = "fixation0123456789abcdefghijklmn"
		planted := http.Header{"Cookie": {sessionCookie + "=" + fixation}}
		newBrowser(t, s).loginIs(t, "the right password, with a session id chosen beforehand", http.StatusSeeOther, statsPasswordValue, planted)
		fixed, err := s.get(ctx, statsPath, planted)
		if err != nil {
			t.Errorf("GET %s: %v", statsPath, err)
		}
		lacks(t, "the session id chosen beforehand is not logged in", loggedIn, string(fixed.body))

		// Caddy keeps the login's handle blocks ahead of stats.php's own only
		// by their order in site.caddy. A GET with op=login reaches login.php,
		// which sends it to the page; upstream would answer it with its form.
		for _, path := range []string{statsPath + "?op=login", statsPath + "?op=x&op=login", statsPath + "?op=login&op=x"} {
			what := fmt.Sprintf("GET %s reaches login.php", path)
			resp, gerr := s.get(ctx, path, nil)
			if gerr != nil {
				checks.True(what, false, gerr.Error())
				continue
			}
			harness.Equal(checks, what, "303 "+s.base+statsPath, fmt.Sprintf("%d %s", resp.status, redirectURL(s.base+path, resp)))
		}

		// Upstream's own check compares a login with stats_password, random
		// and new on every start. Even sent that very value, by any way of
		// writing a login, it decides nothing: every POST is login.php's,
		// whose 403 and 429 upstream never sends, and nobody is logged in.
		generated, err := telemetrySettings(ctx, s.c)
		if err != nil {
			generated = err.Error()
		}
		secret := statsPassword(generated)
		if !hex64.MatchString(secret) {
			checks.True("upstream's stats_password", false, "expected 64 hex digits, got "+clip(generated))
		} else {
			for _, path := range statsPaths {
				fresh := newBrowser(t, s)
				reaches := fmt.Sprintf("POST %s with upstream's stats_password reaches login.php", path)
				if got, err := fresh.login(ctx, path, secret, nil); err != nil {
					checks.True(reaches, false, err.Error())
				} else {
					checks.True(reaches, got.status == http.StatusForbidden || got.status == http.StatusTooManyRequests,
						fmt.Sprintf("%d, where login.php answers 403 or 429", got.status))
				}
				page, err := fresh.page(ctx)
				if err != nil {
					t.Errorf("GET %s: %v", statsPath, err)
				}
				lacks(t, fmt.Sprintf("POST %s with upstream's stats_password logs nobody in", path), loggedIn, page)
			}
		}
		// and a path that starts at stats.php and leaves it never runs it,
		// even posting that same value
		for _, exit := range statsExits {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				leavesStats(t, s, method, exit.path, exit.target, secret)
			}
		}
		records(t, s.probe, "age-failures", "901")
	})

	t.Run("changed", func(t *testing.T) {
		// Written on every start: a new password replaces the old at once.
		// bcrypt alone would also take the new one with anything after its
		// 72nd byte.
		long := strings.Repeat("0", 72)
		s := serve(t, e, image, spec{port: portStatsLogin, volume: volume, env: map[string]string{
			"TELEMETRY":      "true",
			"STATS_PASSWORD": long,
		}})
		b := newBrowser(t, s)
		const what = "after a restart with a new STATS_PASSWORD of 72 bytes: "
		b.loginIs(t, what+"the old one", http.StatusForbidden, statsPasswordValue, nil)
		b.loginIs(t, what+"the new one and a byte more", http.StatusForbidden, long+"x", nil)
		b.loginIs(t, what+"the new one", http.StatusSeeOther, long, nil)
	})
}

// redirectURL is where resp, the answer to a request for target, sends
// the client, as an absolute URL, or "" if it sends it nowhere: what
// curl's %{redirect_url} prints.
func redirectURL(target string, resp response) string {
	location := resp.header.Get("Location")
	if location == "" {
		return ""
	}
	base, err := url.Parse(target)
	if err != nil {
		return location
	}
	u, err := base.Parse(location)
	if err != nil {
		return location
	}
	return u.String()
}
