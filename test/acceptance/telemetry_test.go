// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/divbast/ci/harness"
)

// What a browser at telemetry level basic posts at the end of a test.
// speedtest_worker.js sends FormData, so multipart, with the fields in the
// order [postResult] sends them; the values are as the worker formats
// them, to two decimals.
const (
	resultDL     = "8.42"
	resultUL     = "1.07"
	resultPing   = "23.41"
	resultJitter = "3.18"
	browserUA    = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	// resultClient is the client's address, as X-Forwarded-For carries it
	// to upstream's getClientIp(): one whose ISP has a long name, with
	// characters outside ASCII.
	resultClient = "186.251.0.1"
	// resultJSON is json.php's answer for that result, less its
	// timestamp: the speeds as upstream formats them, and the ISP from
	// the text after the address.
	resultJSON = `{"download":"8.42","upload":"1.07","ping":"23.4","jitter":"3.18","ispinfo":"OnLine Assis Telecomunicações Ltda-EPP, Brazil"}`
	// emptyResultsTable is the table upstream's getPdo() makes, as
	// [resultsTable] reads it, before any result is recorded.
	emptyResultsTable = `{"columns":["id","ispinfo","extra","timestamp","ip","ua","lang","dl","ul","ping","jitter","log"],"rows":0}`
)

// telemetryOn is the environment that turns telemetry on, and nothing
// else.
func telemetryOn() map[string]string { return map[string]string{"TELEMETRY": "true"} }

// TestTelemetry runs telemetry on, recording to SQLite on a volume:
// upstream's table is there before any result, a realistic result is
// recorded and shown as JSON and as the share image, and no other script
// in results/ answers. Restarted on that volume with the client's address
// redacted, the row shows it is gone; a damaged database records nothing,
// and once it is removed upstream makes a new one with the next result.
//
//nolint:paralleltest,tparallel // each phase restarts the server on the volume the one before it wrote, so they run in turn
func TestTelemetry(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	volume := newVolume(t, e)
	// The first result's id, which the restart must still answer
	var first string

	t.Run("recording", func(t *testing.T) {
		s := serve(t, e, image, spec{port: portTelemetry, volume: volume, env: telemetryOn()})
		checks := harness.NewChecks(t)
		ctx := t.Context()

		// Made at start by upstream's own code, before any result: the
		// health check needs it, and upstream would make it only with the
		// first result
		tableIs(t, s.c, "/data/telemetry.db at start: upstream's table, with no rows", emptyResultsTable)
		logContains(t, s.c, "the log says the bootstrap created /data/telemetry.db", "created /data/telemetry.db")
		levelIs(t, s, "settings.json telemetry_level", "basic")
		// The worker posts telemetry_extra in every record, wrapped with
		// the server's name, so the image leaves it to the worker
		if settings, err := settingsOf(ctx, s); err != nil {
			checks.True("settings.json sets no telemetry_extra", false, err.Error())
		} else {
			_, has := settings["telemetry_extra"]
			harness.Equal(checks, "settings.json sets no telemetry_extra", false, has)
		}
		stateIs(t, s.c, "state.json", `{"telemetry":true}`)

		const shaped = "telemetry_settings.php, with stats_password 64 hex digits"
		generated, err := telemetrySettings(ctx, s.c)
		if err != nil {
			checks.True(shaped, false, err.Error())
		} else {
			shape, err := jqEdit(generated, func(fs []field) []field {
				for i, f := range fs {
					if f.key == "stats_password" {
						pw, ok := f.value.(string)
						fs[i].value = ok && hex64.MatchString(pw)
					}
				}
				return fs
			})
			if err != nil {
				checks.True(shaped, false, err.Error())
			} else {
				harness.Equal(checks, shaped, `{"db_type":"sqlite","stats_password":true,"enable_id_obfuscation":false,`+
					`"redact_ip_addresses":false,"Sqlite_db_file":"/data/telemetry.db"}`, shape)
			}
		}
		const unseen = "the log never shows stats_password"
		if password := statsPassword(generated); password != "" {
			logLacks(t, s.c, unseen, password)
		} else {
			checks.True(unseen, false, "there is no stats_password to look for: "+clip(generated))
		}

		var sent int
		first, sent = recordResult(t, s, "a realistic result")
		const fits = "the record fits post_max_size"
		if ini, err := readINI(t, s); err != nil {
			checks.True(fits, false, err.Error())
		} else {
			limit := ini.PostMaxSizeBytes
			checks.True(fits, limit != nil && sent > 0 && int64(sent) < *limit,
				fmt.Sprintf("%d of %s bytes", sent, value(limit)))
		}
		resultIs(t, s, "GET /results/json.php?id="+first, first)
		pngIs(t, s, "GET /results/index.php?id="+first, "/results/index.php?id="+first)
		// The modern UI's share link
		pngIs(t, s, fmt.Sprintf("GET /results/?id=%s&style=modern", first), fmt.Sprintf("/results/?id=%s&style=modern", first))
		isFile(t, s.c, "/data/telemetry.db", "present", telemetryDB)
		rowIs(t, s, fmt.Sprintf("row %s as stored, unredacted", first), first,
			[]string{"ip", "dl", "extra", "log"},
			fmt.Sprintf(`{"ip":%q,"dl":%q,"extra":"{\"server\":\"local\"}","log":"","ip_in_ispinfo":true}`, resultClient, resultDL))

		// No other script in results/ answers, directly or by a dot segment
		// from an allowed route: stats.php has a route only behind its own
		// login, and sanitycheck.php and telemetry_settings.php none at all
		for _, path := range []string{
			"/results/stats.php", "/results/sanitycheck.php", "/results/telemetry_settings.php",
			"/results/telemetry_db.php", "/results/idObfuscation.php", "/results/idObfuscation_salt.php",
			"/results/OpenSans-Light.ttf", "/results/index.html", "/results",
			"/results/json.php/../stats.php", "/results/json.php/%2e%2e/stats.php",
			"/results/telemetry.php/..%2fsanitycheck.php", "/results/index.php/../telemetry_settings.php",
			"/results/index.php/.%2e/telemetry_db.php", "/results/json.php/../../results/stats.php",
			"/results/telemetry.php/../idObfuscation_salt.php",
		} {
			statusIs(t, s, "GET "+path, http.StatusNotFound, path)
		}
		// and a path that resolves onto an allowed route runs that route's
		// script
		answer := fetch(t, s, "/results/json.php?id="+first)
		for _, path := range []string{
			"/results/stats.php/../json.php", "/results/stats.php/%2e%2e/json.php",
			"/results/sanitycheck.php/../json.php", "/results/telemetry_settings.php/.%2e/json.php",
			"/backend/getIP_util.php/../../results/json.php",
		} {
			runsOrRefused(t, s, path+"?id="+first, "json.php", string(answer))
		}
	})

	t.Run("redacted", func(t *testing.T) {
		// json.php never shows the address, so only the row, which rowIs
		// reads, can show it is gone
		s := serve(t, e, image, spec{port: portTelemetry, volume: volume, env: map[string]string{
			"TELEMETRY":           "true",
			"REDACT_IP_ADDRESSES": "true",
		}})
		checks := harness.NewChecks(t)
		ctx := t.Context()

		const redact = "telemetry_settings.php with REDACT_IP_ADDRESSES=true"
		if generated, err := telemetrySettings(ctx, s.c); err != nil {
			checks.True(redact, false, err.Error())
		} else {
			got, err := jqPick(generated, "redact_ip_addresses")
			if err != nil {
				checks.True(redact, false, err.Error())
			} else {
				harness.Equal(checks, redact, `{"redact_ip_addresses":true}`, got)
			}
		}
		redacted, _ := recordResult(t, s, "a result with REDACT_IP_ADDRESSES=true")
		rowIs(t, s, fmt.Sprintf("row %s as stored, redacted", redacted), redacted, []string{"ip"},
			`{"ip":"0.0.0.0","ip_in_ispinfo":false}`)
		resultIs(t, s, fmt.Sprintf("GET /results/json.php?id=%s after the restart", first), first)

		// What the health check sees. A damaged database records nothing;
		// once it is gone, upstream makes a new one with the next result.
		records(t, s.probe, "damage-db")
		got, err := postResult(ctx, s)
		if err != nil {
			checks.True("POST /results/telemetry.php to a damaged database: curl exit, status", false, err.Error())
			checks.True("POST /results/telemetry.php to a damaged database records nothing", false, err.Error())
		} else {
			harness.Equal(checks, "POST /results/telemetry.php to a damaged database: curl exit, status", http.StatusOK, got.status)
			harness.Equal(checks, "POST /results/telemetry.php to a damaged database records nothing", "", got.body)
		}
		records(t, s.probe, "remove-db")
		id, _ := recordResult(t, s, "a result after remove-db")
		harness.Equal(checks, "the first result in a new database", "1", id)
	})
}

// telemetryDB is upstream's database, on the volume.
const telemetryDB = dataDir + "/telemetry.db"

// hex64 is how upstream's stats_password looks: 64 hex digits.
var hex64 = regexp.MustCompile(`\A[0-9a-f]{64}\z`)

// posted is the answer to one result posted to telemetry.php.
type posted struct {
	status int
	// sent is the bytes of the body sent
	sent int
	body string
}

// postResult posts the result a browser would, for [resultClient]: the
// ISP info getIP.php gives that client, as the worker wraps it, then the
// speeds.
func postResult(ctx context.Context, s *served) (posted, error) {
	header := http.Header{"X-Forwarded-For": {resultClient}}
	ip, err := s.get(ctx, "/backend/getIP.php?isp=true&distance=km&r=0.5", header)
	if err != nil {
		return posted{}, err
	}
	// JSON.stringify({processedString, rawIspInfo}) of getIP.php's answer
	var answer struct {
		ProcessedString string `json:"processedString"`
		RawIspInfo      any    `json:"rawIspInfo"`
	}
	if jerr := json.Unmarshal(ip.body, &answer); jerr != nil {
		return posted{}, fmt.Errorf("getIP.php?isp=true answered %d with what is not its JSON: %w: %s", ip.status, jerr, clip(string(ip.body)))
	}
	ispinfo, err := encode(answer)
	if err != nil {
		return posted{}, err
	}
	body, contentType := multipartForm([][2]string{
		{"ispinfo", ispinfo},
		{"dl", resultDL},
		{"ul", resultUL},
		{"ping", resultPing},
		{"jitter", resultJitter},
		{"log", ""},
		{"extra", `{"server":"local"}`},
	})
	header.Set("Content-Type", contentType)
	header.Set("User-Agent", browserUA)
	header.Set("Accept-Language", "pt-BR,pt;q=0.9")
	resp, err := s.do(ctx, harness.HostClient(), http.MethodPost, "/results/telemetry.php?r=0.42", header, body)
	if err != nil {
		return posted{}, err
	}
	return posted{status: resp.status, sent: len(body), body: string(resp.body)}, nil
}

// recordResult posts the result, and checks telemetry.php answers with the
// id it recorded it under, which it returns, with the bytes of the body
// sent. The check's name holds the answer.
func recordResult(t *testing.T, s *served, what string) (id string, sent int) {
	t.Helper()
	checks := harness.NewChecks(t)
	got, err := postResult(t.Context(), s)
	if err != nil {
		checks.True(what+": POST /results/telemetry.php answers 'id <id>'", false, err.Error())
		return "", 0
	}
	id, ok := strings.CutPrefix(got.body, "id ")
	if got.status != http.StatusOK || !ok || id == "" {
		checks.True(what+": POST /results/telemetry.php answers 'id <id>'", false,
			fmt.Sprintf("got %d, %s", got.status, clip(got.body)))
		return "", got.sent
	}
	checks.True(fmt.Sprintf("%s: POST /results/telemetry.php answers '%s'", what, got.body), true,
		fmt.Sprintf("%d, %d bytes sent", got.status, got.sent))
	return id, got.sent
}

// multipartForm is fields as a multipart form, in order, as FormData and
// curl's --form-string send one, and the Content-Type that says so.
func multipartForm(fields [][2]string) (body []byte, contentType string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, f := range fields {
		// A bytes.Buffer takes every write
		_ = w.WriteField(f[0], f[1])
	}
	_ = w.Close()
	return b.Bytes(), w.FormDataContentType()
}

// resultIs checks json.php's answer for id, less the timestamp no test can
// know, is resultJSON.
func resultIs(t *testing.T, s *served, what, id string) {
	t.Helper()
	checks := harness.NewChecks(t)
	resp, err := s.get(t.Context(), "/results/json.php?id="+id, nil)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	got, err := jqEdit(string(resp.body), func(fs []field) []field {
		return slicesDeleteKey(fs, "timestamp")
	})
	if err != nil {
		checks.True(what, false, fmt.Sprintf("%d: %v", resp.status, err))
		return
	}
	harness.Equal(checks, what, resultJSON, got)
}

// pngMagic is the signature every PNG starts with.
const pngMagic = "89504e470d0a1a0a"

// pngIs checks GET path answers a PNG: 200 image/png, starting with PNG's
// signature. index.php answers 200 with an empty body for an id it cannot
// find.
func pngIs(t *testing.T, s *served, what, path string) {
	t.Helper()
	checks := harness.NewChecks(t)
	what += ": status, type, signature"
	resp, err := s.get(t.Context(), path, nil)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	magic := hex.EncodeToString(resp.body[:min(len(resp.body), len(pngMagic)/2)])
	harness.Equal(checks, what, "200 image/png "+pngMagic,
		fmt.Sprintf("%d %s %s", resp.status, resp.header.Get("Content-Type"), magic))
}

// rowIs checks the row id as the probe's read-row reads it: the fields
// named, in that order, then whether its ispinfo holds resultClient, as
// ip_in_ispinfo.
func rowIs(t *testing.T, s *served, what, id string, keys []string, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	row, err := output(t.Context(), s.probe, "read-row", id)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	got, err := jqEdit(row, func(fs []field) []field {
		shape := make([]field, 0, len(keys)+1)
		for _, k := range keys {
			shape = append(shape, field{key: k, value: lookup(fs, k)})
		}
		ispinfo, ok := lookup(fs, "ispinfo").(string)
		return append(shape, field{key: "ip_in_ispinfo", value: ok && strings.Contains(ispinfo, resultClient)})
	})
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, got)
}

// telemetrySettings returns, as JSON, the variables of the generated
// telemetry_settings.php, as upstream's scripts see them once they require
// it.
func telemetrySettings(ctx context.Context, c *harness.Container) (string, error) {
	return phpIn(ctx, c, `require "`+generatedDir+`/telemetry_settings.php";
		echo json_encode(compact("db_type", "stats_password", "enable_id_obfuscation",
			"redact_ip_addresses", "Sqlite_db_file")), "\n";`)
}

// statsPassword returns the stats_password telemetrySettings printed, or
// "" if there is none.
func statsPassword(generated string) string {
	var settings struct {
		StatsPassword string `json:"stats_password"`
	}
	if json.Unmarshal([]byte(generated), &settings) != nil {
		return ""
	}
	return settings.StatsPassword
}

// tableIs checks the results table in c's database, as JSON: its columns
// and how many rows it holds. It opens the database read-only, so it never
// creates one.
func tableIs(t *testing.T, c *harness.Container, what, want string) {
	t.Helper()
	checks := harness.NewChecks(t)
	got, err := phpIn(t.Context(), c, `function db(): PDO { return new PDO("sqlite:`+telemetryDB+`", null, null,
			[PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, Pdo\Sqlite::ATTR_OPEN_FLAGS => Pdo\Sqlite::OPEN_READONLY]); }
		echo json_encode(["columns" => array_column(db()->query("PRAGMA table_info(speedtest_users)")->fetchAll(), "name"),
			"rows" => (int) db()->query("SELECT COUNT(*) FROM speedtest_users")->fetchColumn()]), "\n";`)
	if err != nil {
		checks.True(what, false, err.Error())
		return
	}
	harness.Equal(checks, what, want, strings.TrimSuffix(got, "\n"))
}

// field is one member of a JSON object, in the order it came.
type field struct {
	key   string
	value any
}

// jqEdit reads raw as a JSON object, edits its members, and writes them
// back as jq -c would: compact, in the order edit leaves them, and with
// no escape for text outside ASCII. Numbers keep the text they came as.
func jqEdit(raw string, edit func([]field) []field) (string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", fmt.Errorf("not a JSON object: %s", clip(raw))
	}
	var fs []field
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("reading a JSON object: %w: %s", err, clip(raw))
		}
		key, _ := tok.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			return "", fmt.Errorf("reading a JSON object: %w: %s", err, clip(raw))
		}
		fs = append(fs, field{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return "", fmt.Errorf("reading a JSON object: %w: %s", err, clip(raw))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("more than one JSON value: %s", clip(raw))
	}

	var b strings.Builder
	b.WriteByte('{')
	for i, f := range edit(fs) {
		k, err := encode(f.key)
		if err != nil {
			return "", err
		}
		v, err := encode(f.value)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + ":" + v)
	}
	b.WriteByte('}')
	return b.String(), nil
}

// jqPick is jq -c '{key}': raw's member key alone, null if it has none.
func jqPick(raw, key string) (string, error) {
	return jqEdit(raw, func(fs []field) []field { return []field{{key: key, value: lookup(fs, key)}} })
}

// lookup returns the value of the member key, or nil, as jq gives null,
// if there is none.
func lookup(fs []field, key string) any {
	for _, f := range fs {
		if f.key == key {
			return f.value
		}
	}
	return nil
}

// slicesDeleteKey returns fs without the member key.
func slicesDeleteKey(fs []field, key string) []field {
	return slices.DeleteFunc(fs, func(f field) bool { return f.key == key })
}

// encode writes v as compact JSON, as jq and JSON.stringify do: no escape
// for <, > and &.
func encode(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
