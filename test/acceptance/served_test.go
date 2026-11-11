// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
	"github.com/divbast/librespeed-oci-image/test/probe"
)

// What LibreSpeed's contract says of every container here.
const (
	// tmpfsSize is the size of /tmp the README states. Every container
	// gets exactly this, so no check passes on more room than the
	// contract grants.
	tmpfsSize = "16m"
	// appPort serves the speed test, and healthPort only /healthz.
	appPort    = 8080
	healthPort = 8081
)

// healthCmd is the shipped health probe, as the image's HEALTHCHECK runs
// it and a Podman user passes it with --health-cmd.
var healthCmd = []string{harness.FrankenPHP, "php-cli", "/usr/local/bin/librespeed-health"}

// serveWithin bounds how long a container may take to serve, as the bash
// suite bounded it, and attemptWithin each request while it waits.
const (
	serveWithin   = time.Minute
	attemptWithin = 5 * time.Second
)

// getIPLoopback is what getIP.php answers a client on loopback.
const getIPLoopback = `{"processedString":"127.0.0.1","rawIspInfo":""}`

// The host port each test that publishes the speed test publishes it on.
// The tests run at once, so no two share one.
const (
	portNoData = 18290 + iota
	portINI
	portRoutes
	portDotSegments
	portEndpoints
	portUploads
	portParallelUploads
	portHostile
)

// served is a container serving the speed test under the contract, with
// its app port published on the host's loopback.
type served struct {
	c *harness.Container
	// probe is installed once the server is serving.
	probe *harness.Probe
	// base is the speed test's URL from the host.
	base string
}

// start starts image under the contract, on a network of its own, with
// env set and the app port published on hostPort. It returns once the
// engine has started the container; [served.wait] waits for the server.
func start(t *testing.T, e harness.Engine, image string, hostPort int, env map[string]string) (*served, error) {
	t.Helper()
	ctx := t.Context()
	network, err := e.Network(ctx, t, "", "", false)
	if err != nil {
		return nil, err
	}
	c, err := e.Start(ctx, t, &harness.RunSpec{
		Image:   image,
		Network: network,
		Confine: harness.Contract(tmpfsSize),
		Env:     env,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:%d", hostPort, appPort)},
	})
	if err != nil {
		return nil, err
	}
	return &served{c: c, base: fmt.Sprintf("http://127.0.0.1:%d", hostPort)}, nil
}

// serve starts image as [start] does, then waits for GET / to answer. It
// fails t at once if the server never serves, since every check after
// would fail for that one reason.
func serve(t *testing.T, e harness.Engine, image string, hostPort int, env map[string]string) *served {
	t.Helper()
	s, err := start(t, e, image, hostPort, env)
	if err == nil {
		err = s.wait(t.Context(), "/")
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wait asks for GET path from the host once a second until any status
// comes back, then installs the probe. A container that stops first, or
// one still not serving after serveWithin, is an error carrying its log,
// so a server that died says why.
func (s *served) wait(ctx context.Context, path string) error {
	deadline := time.Now().Add(serveWithin)
	for {
		attempt, cancel := context.WithTimeout(ctx, attemptWithin)
		_, err := s.get(attempt, path, nil)
		cancel()
		if err == nil {
			break
		}
		ins, ierr := s.c.Inspect(ctx)
		switch {
		case ierr != nil:
			return fmt.Errorf("%w; and then: %w", err, ierr)
		case !ins.State.Running:
			return withLog(ctx, s.c, fmt.Errorf("container %s stopped, exit status %d, before GET %s answered", s.c, ins.State.ExitCode, path))
		case !time.Now().Before(deadline):
			return withLog(ctx, s.c, fmt.Errorf("GET %s%s got no answer within %s: %w", s.base, path, serveWithin, err))
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
	p, err := harness.InstallProbe(ctx, s.c, probe.App)
	if err != nil {
		return err
	}
	s.probe = p
	return nil
}

// response is the answer to one request from the host.
type response struct {
	status int
	header http.Header
	body   []byte
}

// request returns a request from the host for path, its query included,
// which goes out exactly as written, as curl's --path-as-is sends it: no
// dot segment resolved and no escape undone. A path Go would send any
// other way is an error.
func (s *served) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return nil, err
	}
	if sent := req.URL.RequestURI(); sent != path {
		return nil, fmt.Errorf("%s %s would go out as %s", method, path, sent)
	}
	return req, nil
}

// get sends GET path from the host, with header, and reads the answer.
func (s *served) get(ctx context.Context, path string, header http.Header) (response, error) {
	req, err := s.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return response{}, err
	}
	maps.Copy(req.Header, header)
	return send(harness.HostClient(), req)
}

// send sends req with client, and reads the whole answer.
func send(client *http.Client, req *http.Request) (response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	body, err := io.ReadAll(resp.Body)
	if err = errors.Join(err, resp.Body.Close()); err != nil {
		return response{}, fmt.Errorf("%s %s: reading the answer: %w", req.Method, req.URL.RequestURI(), err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// statusIs checks GET path, from the host, answers want.
func statusIs(t *testing.T, s *served, what string, want int, path string) {
	t.Helper()
	checks := harness.NewChecks(t)
	resp, err := s.get(t.Context(), path, nil)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, resp.status)
}

// contains checks text holds needle.
func contains(t *testing.T, what, needle, text string) {
	t.Helper()
	ok := strings.Contains(text, needle)
	detail := fmt.Sprintf("holds %q", needle)
	if !ok {
		detail = fmt.Sprintf("%q is not in %s", needle, clip(text))
	}
	harness.NewChecks(t).True(what, ok, detail)
}

// lacks checks text does not hold needle. Empty text would lack anything,
// so it fails rather than passes.
func lacks(t *testing.T, what, needle, text string) {
	t.Helper()
	ok := text != "" && !strings.Contains(text, needle)
	var detail string
	switch {
	case text == "":
		detail = "there is nothing to look through"
	case ok:
		detail = fmt.Sprintf("no %q in %d bytes", needle, len(text))
	default:
		detail = fmt.Sprintf("%q is in %s", needle, clip(text))
	}
	harness.NewChecks(t).True(what, ok, detail)
}

// clipAt is as much of a text as a failing check quotes.
const clipAt = 600

// clip quotes text, cut short if it is long.
func clip(text string) string {
	if len(text) <= clipAt {
		return fmt.Sprintf("%q", text)
	}
	return fmt.Sprintf("%q... (%d bytes)", text[:clipAt], len(text))
}

// loopbackGet sends GET path to the speed test from inside its container,
// through the probe, once a second until it answers 200 or serveWithin
// passes, and returns the body.
func loopbackGet(ctx context.Context, p *harness.Probe, path string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", appPort, path)
	deadline := time.Now().Add(serveWithin)
	for {
		resp, err := p.HTTP(ctx, http.MethodGet, url, nil, "")
		if err == nil && resp.Status == http.StatusOK {
			return string(resp.Body), nil
		}
		if err == nil {
			err = fmt.Errorf("GET %s from inside the container answered %d: %s", url, resp.Status, clip(string(resp.Body)))
		}
		if !time.Now().Before(deadline) {
			return "", err
		}
		if err := pause(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

// accessLogWithin bounds the wait for a request's line in the access log,
// which may land a moment after the response does.
const accessLogWithin = 5 * time.Second

// bytesRead returns how many bytes of the request for uri the server read
// of its body, from the app port's access log.
func bytesRead(ctx context.Context, c *harness.Container, uri string) (int64, error) {
	deadline := time.Now().Add(accessLogWithin)
	for {
		logs, err := c.Logs(ctx)
		if err != nil {
			return 0, err
		}
		for line := range strings.Lines(logs) {
			var entry struct {
				Request struct {
					URI string `json:"uri"`
				} `json:"request"`
				BytesRead *int64 `json:"bytes_read"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Request.URI == uri && entry.BytesRead != nil {
				return *entry.BytesRead, nil
			}
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("no access log line for %s", uri)
		}
		if err := pause(ctx, time.Second); err != nil {
			return 0, err
		}
	}
}

// phpIn runs PHP code in c and returns what it printed, stdout then
// stderr. A non-zero exit status is an error.
func phpIn(ctx context.Context, c *harness.Container, code string) (string, error) {
	res, err := c.Exec(ctx, nil, harness.FrankenPHP, "php-cli", "-r", code)
	if err != nil {
		return "", err
	}
	out := string(res.Stdout) + string(res.Stderr)
	if res.Code != 0 {
		return "", fmt.Errorf("php-cli -r in container %s: exit status %d: %s", c, res.Code, strings.TrimSpace(out))
	}
	return out, nil
}

// withLog adds c's log to err.
func withLog(ctx context.Context, c *harness.Container, err error) error {
	logs, lerr := c.Logs(ctx)
	if lerr != nil {
		return fmt.Errorf("%w; its log could not be read: %w", err, lerr)
	}
	return fmt.Errorf("%w; its log:\n%s", err, logs)
}

// pause waits for d, or until ctx is done.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
