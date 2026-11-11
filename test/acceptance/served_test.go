// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
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

// serverArgv is the server's argv: what the bootstrap hands over to, with
// pcntl_exec(SERVER_BIN, ['run', '--config', CADDYFILE]).
var serverArgv = []string{harness.FrankenPHP, "run", "--config", "/etc/frankenphp/Caddyfile"}

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

// The host port each test that publishes the speed test publishes it on,
// and the health port where a test publishes that too. The tests run at
// once, so no two share one.
const (
	portNoData = 18290 + iota
	portINI
	portRoutes
	portDotSegments
	portEndpoints
	portUploads
	portParallelUploads
	portHostile
	portTelemetryOff
	portSwitchesIgnored
	portTelemetry
	portSalt
	portStatsOff
	portStatsIgnored
	portStatsLogin
	portLockout
	portLockRelease
	portProxyList
	portHealthOff
	portHealthOffHealth
	portHealthOn
	portHealthOnHealth
)

// served is a container serving the speed test under the contract, with
// its app port published on the host's loopback.
type served struct {
	c *harness.Container
	// probe is installed once the server is serving.
	probe *harness.Probe
	// base is the speed test's URL from the host, and healthBase the
	// health port's, when it is published.
	base, healthBase string
}

// spec is how a test runs a container, beyond the contract every
// container here runs under.
type spec struct {
	// port is the host port the app port is published on. healthPort,
	// when set, is the one the health port is published on, which a
	// deployment keeps unpublished; it also has Podman run the health
	// check, which a Podman user is told to pass as --health-cmd.
	port, healthPort int
	env              map[string]string
	// volume, when set, is a volume the test made, mounted at /data.
	volume string
	// network, when set, is a network the test made, and ip a fixed
	// address on it. Unset, the container gets a network of its own.
	network, ip string
}

// start starts image under the contract, as sp says. It returns once the
// engine has started the container; [served.wait] waits for the server.
func start(t *testing.T, e harness.Engine, image string, sp spec) (*served, error) {
	t.Helper()
	ctx := t.Context()
	network := sp.network
	if network == "" {
		var err error
		if network, err = e.Network(ctx, t, "", "", false); err != nil {
			return nil, err
		}
	}
	run := &harness.RunSpec{
		Image:   image,
		Network: network,
		IP:      sp.ip,
		Confine: harness.Contract(tmpfsSize),
		Env:     sp.env,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:%d", sp.port, appPort)},
	}
	if sp.volume != "" {
		run.Mounts = []harness.Mount{{Source: sp.volume, Target: dataDir, Volume: true}}
	}
	s := &served{base: fmt.Sprintf("http://127.0.0.1:%d", sp.port)}
	if sp.healthPort != 0 {
		run.Publish = append(run.Publish, fmt.Sprintf("127.0.0.1:%d:%d", sp.healthPort, healthPort))
		run.HealthCmd = healthCmd
		s.healthBase = fmt.Sprintf("http://127.0.0.1:%d", sp.healthPort)
	}
	c, err := e.Start(ctx, t, run)
	if err != nil {
		return nil, err
	}
	s.c = c
	return s, nil
}

// serve starts image as [start] does, then waits for GET / to answer. It
// fails t at once if the server never serves, since every check after
// would fail for that one reason.
func serve(t *testing.T, e harness.Engine, image string, sp spec) *served {
	t.Helper()
	s, err := start(t, e, image, sp)
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
	return s.do(ctx, harness.HostClient(), http.MethodGet, path, header, nil)
}

// formType is how a browser posts a form, and how upstream's login form
// and curl's --data-urlencode send one.
const formType = "application/x-www-form-urlencoded"

// postForm sends POST path from the host with client, header and form,
// and reads the answer.
func (s *served) postForm(ctx context.Context, client *http.Client, path string, header http.Header, form url.Values) (response, error) {
	h := http.Header{"Content-Type": {formType}}
	maps.Copy(h, header)
	return s.do(ctx, client, http.MethodPost, path, h, []byte(form.Encode()))
}

// do sends method path from the host with client, header and body, and
// reads the answer. header goes out as written: a name Go would put in
// canonical form keeps the case it was given.
func (s *served) do(ctx context.Context, client *http.Client, method, path string, header http.Header, body []byte) (response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := s.request(ctx, method, path, r)
	if err != nil {
		return response{}, err
	}
	maps.Copy(req.Header, header)
	return send(client, req)
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
	resp, err := s.get(t.Context(), path, nil)
	answers(t, what, want, resp, err)
}

// answers checks a request got an answer, and that its status is want.
func answers(t *testing.T, what string, want int, resp response, err error) {
	t.Helper()
	checks := harness.NewChecks(t)
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
	target := fmt.Sprintf("http://127.0.0.1:%d%s", appPort, path)
	deadline := time.Now().Add(serveWithin)
	for {
		resp, err := p.HTTP(ctx, http.MethodGet, target, nil, "")
		if err == nil && resp.Status == http.StatusOK {
			return string(resp.Body), nil
		}
		if err == nil {
			err = fmt.Errorf("GET %s from inside the container answered %d: %s", target, resp.Status, clip(string(resp.Body)))
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

// dataDir is the volume TELEMETRY=true needs: everything that outlives a
// start lives there.
const dataDir = "/data"

// newVolume makes a volume for t, removed when t ends, or fails t.
func newVolume(t *testing.T, e harness.Engine) string {
	t.Helper()
	v, err := e.Volume(t.Context(), t, "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// newNetwork makes a network for t with the subnet given, removed when t
// ends, or fails t. A container run again on it at a fixed address is the
// same client to a server it connects to, or that its published port
// hands on: rootless Podman hands the server a published port's
// connections from the server's own address, where Docker uses the
// gateway's, and a container run again otherwise draws a new one.
func newNetwork(t *testing.T, e harness.Engine, subnet string) string {
	t.Helper()
	n, err := e.Network(t.Context(), t, "", subnet, false)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// phpOnVolume runs PHP code in a throwaway container of image, under the
// contract, so as uid 65532, the volume's owner, with volume at /data. A
// failure fails t at once: the checks after it would test a volume other
// than the one they name.
func phpOnVolume(t *testing.T, e harness.Engine, image, volume, code string) {
	t.Helper()
	res, err := e.Run(t.Context(), t, &harness.RunSpec{
		Image:      image,
		Network:    "none",
		Confine:    harness.Contract(tmpfsSize),
		Mounts:     []harness.Mount{{Source: volume, Target: dataDir, Volume: true}},
		Entrypoint: []string{harness.FrankenPHP},
		Args:       []string{"php-cli", "-r", code},
	})
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit status %d: %s", res.Code, strings.TrimSpace(string(res.Stdout)+string(res.Stderr)))
	}
	if err != nil {
		t.Fatalf("PHP on volume %s: %v", volume, err)
	}
}

// refusal is how [refuses] runs a container that must not start, beyond
// the contract.
type refusal struct {
	env map[string]string
	// volume, when set, is mounted at /data.
	volume string
	// extra holds raw run flags, for a /data no field of the harness
	// makes: a tmpfs root owns, as a host directory would be.
	extra []string
}

// refuses runs image as r says and checks it refuses to start: the
// container exits 1, and its log says needle. It runs on no network, since
// the bootstrap refuses before there is a server to reach.
func refuses(t *testing.T, e harness.Engine, image, what, needle string, r refusal) {
	t.Helper()
	ctx := t.Context()
	checks := harness.NewChecks(t)
	exits, says := what+": the container exits", fmt.Sprintf("%s: the log says '%s'", what, needle)
	run := &harness.RunSpec{
		Image:   image,
		Network: "none",
		Confine: harness.Contract(tmpfsSize),
		Env:     r.env,
		Extra:   r.extra,
	}
	if r.volume != "" {
		run.Mounts = []harness.Mount{{Source: r.volume, Target: dataDir, Volume: true}}
	}
	c, err := e.Start(ctx, t, run)
	if err != nil {
		checks.True(exits, false, err.Error())
		checks.True(says, false, err.Error())
		return
	}
	if code, werr := c.WaitExit(ctx, refuseWithin); werr != nil {
		checks.True(exits, false, werr.Error())
	} else {
		harness.Equal(checks, exits, 1, code)
	}
	logs, err := c.Logs(ctx)
	if err != nil {
		checks.True(says, false, err.Error())
		return
	}
	contains(t, says, needle, logs)
}

// output runs the probe subcommand sub in p, one that prints what it
// reads or does rather than records, and returns what it printed. A
// non-zero exit status is an error, with what the probe said.
func output(ctx context.Context, p *harness.Probe, sub string, args ...string) (string, error) {
	res, err := p.Output(ctx, sub, args...)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("probe %s: exit status %d: %s", sub, res.Code, strings.TrimSpace(string(res.Stdout)+string(res.Stderr)))
	}
	return string(res.Stdout), nil
}

// records runs the probe subcommand sub in p and reports its records,
// each a check of its own.
func records(t *testing.T, p *harness.Probe, sub string, args ...string) {
	t.Helper()
	recs, err := p.Run(t.Context(), sub, args...)
	if err != nil {
		t.Errorf("FAIL %v", err)
		return
	}
	harness.NewChecks(t).Records(recs)
}

// fileState says whether path is in c: present or absent.
func fileState(ctx context.Context, c *harness.Container, path string) (string, error) {
	return phpIn(ctx, c, fmt.Sprintf(`echo file_exists(%q) ? "present" : "absent";`, path))
}

// isFile checks path in c is present or absent, as want says.
func isFile(t *testing.T, c *harness.Container, what, want, path string) {
	t.Helper()
	checks := harness.NewChecks(t)
	got, err := fileState(t.Context(), c, path)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, got)
}

// logContains checks c's log holds needle, and logLacks that it does not.
func logContains(t *testing.T, c *harness.Container, what, needle string) {
	t.Helper()
	logs, err := c.Logs(t.Context())
	if err != nil {
		harness.NewChecks(t).True(what, false, err.Error())
		return
	}
	contains(t, what, needle, logs)
}

func logLacks(t *testing.T, c *harness.Container, what, needle string) {
	t.Helper()
	logs, err := c.Logs(t.Context())
	if err != nil {
		harness.NewChecks(t).True(what, false, err.Error())
		return
	}
	lacks(t, what, needle, logs)
}

// generatedDir is where the bootstrap writes, on every start, what the
// server serves and reads that changes from one start to the next.
const generatedDir = "/tmp/librespeed"

// generated returns the file the bootstrap generated as name, as the
// running server reads it.
func generated(ctx context.Context, c *harness.Container, name string) (string, error) {
	return phpIn(ctx, c, fmt.Sprintf(`readfile(%q);`, generatedDir+"/"+name))
}

// stateIs checks state.json, what the bootstrap turned on for the health
// check, is want, written as jq -c writes it.
func stateIs(t *testing.T, c *harness.Container, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	raw, err := generated(t.Context(), c, "state.json")
	if err == nil {
		raw, err = jqEdit(raw, func(fs []field) []field { return fs })
	}
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, raw)
}

// settingsOf returns settings.json, as the UI reads it.
func settingsOf(ctx context.Context, s *served) (map[string]any, error) {
	resp, err := s.get(ctx, "/settings.json", nil)
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(resp.body, &settings); err != nil {
		return nil, fmt.Errorf("GET /settings.json answered %d with what is not a JSON object: %w: %s", resp.status, err, clip(string(resp.body)))
	}
	return settings, nil
}

// levelIs checks settings.json's telemetry_level is want.
func levelIs(t *testing.T, s *served, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	settings, err := settingsOf(t.Context(), s)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, fmt.Sprint(settings["telemetry_level"]))
}
