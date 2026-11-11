// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// What login.php answers when the login itself fails.
const loginBroken = "The login is not working"

// dbHoldSeconds outlasts login.php's own 10 s wait for the database, so a
// login sent during the hold fails with an exception rather than waiting
// it out.
const dbHoldSeconds = "15"

// TestLockReleasedAfterException checks login.php lets go of its lock when
// the check fails with an exception while it holds it, as one does when
// the database stays locked past login.php's wait. A lock that outlived
// the request would turn every later login away as busy.
func TestLockReleasedAfterException(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{port: portLockRelease, volume: newVolume(t, e), env: statsOn()})
	checks := harness.NewChecks(t)
	ctx := t.Context()
	b := newBrowser(t, s)

	// One failure makes login.php's table, so the exception can only come
	// once it holds the lock: making the table needs the write lock too
	b.loginIs(t, "a wrong password, which makes login.php's database", http.StatusForbidden, "wrong", nil)

	type held struct {
		out string
		err error
	}
	hold := make(chan held, 1)
	go func() {
		out, err := output(ctx, s.probe, "hold-db-lock", dbHoldSeconds)
		hold <- held{out: out, err: err}
	}()
	locked, err := pollHolders(ctx, s.probe, statsLoginDB, 10*time.Second, func(h holder) bool {
		return h.kind == "POSIX" && h.access == "WRITE" && slices.Contains(h.argv, "hold-db-lock")
	})
	lockSeen(t, "hold-db-lock holds the database's write lock", locked, err)

	type answered struct {
		resp response
		err  error
	}
	login := make(chan answered, 1)
	go func() {
		client := harness.HostClient()
		client.Timeout = time.Minute
		resp, perr := s.postForm(ctx, client, loginPath, nil, map[string][]string{"password": {statsPasswordValue}})
		login <- answered{resp: resp, err: perr}
	}()
	// The login takes its lock, then waits for the database
	taken, err := pollHolders(ctx, s.probe, statsLoginLock, 10*time.Second, func(h holder) bool {
		return h.kind == "FLOCK" && h.access == "WRITE" && slices.Equal(h.argv, serverArgv)
	})
	lockSeen(t, "while the database is locked, the right password takes login.php's lock", taken, err)

	got := <-login
	const fails = "the right password, while the database stays locked: login.php fails with an exception"
	if got.err != nil {
		checks.True(fails, false, got.err.Error())
	} else {
		harness.Equal(checks, fails, http.StatusInternalServerError, got.resp.status)
		contains(t, "login.php says the login is not working", loginBroken, string(got.resp.body))
	}
	logContains(t, s.c, "the server log says why: the database is locked", "librespeed login: SQLSTATE[HY000]: General error: 5 database is locked")

	// PHP closes the lock's file as the request ends, which may be a moment
	// after the answer is in
	left, err := untilFree(ctx, s.probe, statsLoginLock, 5*time.Second)
	if err != nil {
		checks.True("after the exception, login.php's lock is free", false, err.Error())
	} else {
		checks.True("after the exception, login.php's lock is free", len(left) == 0, fmt.Sprintf("still held: %v", left))
	}

	if h := <-hold; h.err != nil || h.out != "held\nreleased\n" {
		t.Errorf("hold-db-lock %s: %v, printed %q", dbHoldSeconds, h.err, h.out)
	}
	b.loginIs(t, "once the database is free, the right password", http.StatusSeeOther, statsPasswordValue, nil)
}

// lockSeen checks a poll for a lock found one, and says which, or why it
// found none.
func lockSeen(t *testing.T, what string, found []holder, err error) {
	t.Helper()
	checks := harness.NewChecks(t)
	switch {
	case err != nil:
		checks.True(what, false, err.Error())
	case len(found) == 0:
		checks.True(what, false, "no such lock was held within 10 s")
	default:
		checks.True(what, true, fmt.Sprint(found))
	}
}

// untilFree asks for the locks on path until none is held, or within has
// passed, and returns those still held then.
func untilFree(ctx context.Context, p *harness.Probe, path string, within time.Duration) ([]holder, error) {
	deadline := time.Now().Add(within)
	for {
		holders, err := lockHolders(ctx, p, path)
		if err != nil || len(holders) == 0 || !time.Now().Before(deadline) {
			return holders, err
		}
		if err := pause(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
}
