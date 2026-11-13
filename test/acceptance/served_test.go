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

// getIPLoopback is what getIP.php answers a client on loopback.
const getIPLoopback = `{"processedString":"127.0.0.1","rawIspInfo":""}`

// spec is how a test runs a container, beyond the contract every
// container here runs under.
type spec struct {
	env map[string]string
	// volume, when set, is a volume the test made, mounted at /data.
	volume string
	// network, when set, is a network the test made, and ip a fixed
	// address on it. Unset, the container gets a network of its own.
	network, ip string
	// health publishes the health port too, which a deployment keeps
	// unpublished. It also has Podman run the health check, which a Podman
	// user is told to pass as --health-cmd.
	health bool
}

// serveSpec is sp as [harness.Engine.Serve] takes it: image under the
// contract, and the probe with LibreSpeed's own subcommands. Serve
// publishes the app port, and the health port when sp says so, on the
// host's loopback at a port the engine picks, so the tests, which run at
// once, never ask for the same one.
func (sp spec) serveSpec(image string) *harness.ServeSpec {
	ss := &harness.ServeSpec{
		Run: harness.RunSpec{
			Image:   image,
			Network: sp.network,
			IP:      sp.ip,
			Confine: harness.Contract(tmpfsSize),
			Env:     sp.env,
		},
		AppPort:  appPort,
		ProbeApp: probe.App,
	}
	if sp.volume != "" {
		ss.Run.Mounts = onData(sp.volume)
	}
	if sp.health {
		ss.HealthPort = healthPort
		ss.Run.HealthCmd = healthCmd
	}
	return ss
}

// serve serves image as sp says, with [harness.Engine.Serve]: it returns
// once GET / answers and the probe is installed. It fails t at once if
// the server never serves, since every check after would fail for that
// one reason.
func serve(t *testing.T, e harness.Engine, image string, sp spec) *harness.Serving {
	t.Helper()
	s, err := e.Serve(t.Context(), t, sp.serveSpec(image))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// request returns a request from the host for path on s's app port, its
// query included, which goes out exactly as written, as
// [harness.Serving.Do] sends one: no dot segment resolved and no escape
// undone. A path Go would send any other way is an error. It is for a
// body Do cannot send, one streamed as the client reads it.
func request(ctx context.Context, s *harness.Serving, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.Base+path, body)
	if err != nil {
		return nil, err
	}
	if sent := req.URL.RequestURI(); sent != path {
		return nil, fmt.Errorf("%s %s would go out as %s", method, path, sent)
	}
	return req, nil
}

// send sends req with client, and reads the whole answer.
func send(client *http.Client, req *http.Request) (harness.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return harness.Response{}, err
	}
	body, err := io.ReadAll(resp.Body)
	if err = errors.Join(err, resp.Body.Close()); err != nil {
		return harness.Response{}, fmt.Errorf("%s %s: reading the answer: %w", req.Method, req.URL.RequestURI(), err)
	}
	return harness.Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// formType is how a browser posts a form, and how upstream's login form
// and curl's --data-urlencode send one.
const formType = "application/x-www-form-urlencoded"

// postForm sends POST path to s from the host with client, header and
// form, and reads the answer.
func postForm(ctx context.Context, s *harness.Serving, client *http.Client, path string, header http.Header, form url.Values) (harness.Response, error) {
	h := http.Header{"Content-Type": {formType}}
	maps.Copy(h, header)
	return s.Do(ctx, client, http.MethodPost, path, h, []byte(form.Encode()))
}

// loopbackGet sends GET path to the speed test from inside its container,
// through the probe, once a second until it answers 200 or
// harness.ServeWithin passes, and returns the body.
func loopbackGet(ctx context.Context, p *harness.Probe, path string) (string, error) {
	target := fmt.Sprintf("http://127.0.0.1:%d%s", appPort, path)
	deadline := time.Now().Add(harness.ServeWithin)
	for {
		resp, err := p.HTTP(ctx, http.MethodGet, target, nil, "")
		if err == nil && resp.Status == http.StatusOK {
			return string(resp.Body), nil
		}
		if err == nil {
			err = fmt.Errorf("GET %s from inside the container answered %d: %s", target, resp.Status, harness.Clip(string(resp.Body)))
		}
		if !time.Now().Before(deadline) {
			return "", err
		}
		if err := harness.Pause(ctx, time.Second); err != nil {
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
		if err := harness.Pause(ctx, time.Second); err != nil {
			return 0, err
		}
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

// newNetwork makes a network for t, on a subnet the engine picks, removed
// when t ends, or fails t. No subnet is fixed, so none can clash with one
// another suite, or the host, already has. A container run again on it at
// the address it had before, which [addressOf] reads, is the same client
// to a server it connects to, or that its published port hands on:
// rootless Podman hands the server a published port's connections from
// the server's own address, where Docker uses the gateway's, and a
// container run again otherwise draws a new one.
func newNetwork(t *testing.T, e harness.Engine) string {
	t.Helper()
	n, err := e.Network(t.Context(), t, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// addressOf returns c's address on the network it was started on, as the
// engine reports it, or fails t.
func addressOf(t *testing.T, c *harness.Container) string {
	t.Helper()
	ins, err := c.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ins.IP == "" {
		t.Fatalf("container %s has no address on the network it was started on", c)
	}
	return ins.IP
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
		Mounts:     onData(volume),
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

// onData mounts volume at /data.
func onData(volume string) []harness.Mount {
	return []harness.Mount{{Source: volume, Target: dataDir, Volume: true}}
}

// refusedExit is the exit status the bootstrap refuses a start with.
const refusedExit = 1

// refusing is how [harness.Refuses] runs image with env, as a container
// that must refuse to start: under the contract, and on no network, since
// the bootstrap refuses before there is a server to reach.
func refusing(image string, env map[string]string) *harness.RunSpec {
	return &harness.RunSpec{
		Image:   image,
		Network: "none",
		Confine: harness.Contract(tmpfsSize),
		Env:     env,
	}
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

// generatedDir is where the bootstrap writes, on every start, what the
// server serves and reads that changes from one start to the next.
const generatedDir = "/tmp/librespeed"

// generated returns the file the bootstrap generated as name, as the
// running server reads it.
func generated(ctx context.Context, c *harness.Container, name string) (string, error) {
	return harness.PHPIn(ctx, c, fmt.Sprintf(`readfile(%q);`, generatedDir+"/"+name))
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
func settingsOf(ctx context.Context, s *harness.Serving) (map[string]any, error) {
	resp, err := s.Get(ctx, "/settings.json", nil)
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(resp.Body, &settings); err != nil {
		return nil, fmt.Errorf("GET /settings.json answered %d with what is not a JSON object: %w: %s", resp.Status, err, harness.Clip(string(resp.Body)))
	}
	return settings, nil
}

// levelIs checks settings.json's telemetry_level is want.
func levelIs(t *testing.T, s *harness.Serving, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	settings, err := settingsOf(t.Context(), s)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, fmt.Sprint(settings["telemetry_level"]))
}
