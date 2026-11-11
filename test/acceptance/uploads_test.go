// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/divbast/ci/harness"
)

// uploadBytes is the speed test's upload: speedtest_worker.js posts blobs
// of xhr_ul_blob_megabytes, 20 by default, each of 1048576 bytes. It is
// more than the 16 MiB tmpfs holds, so a server that put it on /tmp would
// fail it.
const uploadBytes = 20 * 1048576

// uploadWithin bounds one upload, as the bash suite's curl did.
const uploadWithin = time.Minute

// uploaded is how one upload went: the status that came back and the
// bytes of the body the client sent, or why it never completed.
type uploaded struct {
	status int
	sent   int64
	err    error
}

// TestUploads posts the speed test's upload as a typed body and as the
// untyped Blob browsers send, and checks each arrives whole.
func TestUploads(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{port: portUploads})

	for _, tt := range []struct {
		tag, what, contentType string
	}{
		{tag: "6-typed", what: "POST /backend/empty.php, 20 MiB of application/octet-stream", contentType: "application/octet-stream"},
		// XMLHttpRequest sends speedtest_worker.js's untyped Blob with no
		// Content-Type
		{tag: "6-untyped", what: "POST /backend/empty.php, 20 MiB with no Content-Type, as browsers send it"},
	} {
		checkUpload(t, s, tt.tag, tt.what, s.upload(t.Context(), tt.tag, tt.contentType))
	}
}

// TestParallelUploads posts three uploads at once, 60 MiB in all, to a
// server whose /tmp holds 16 MiB, and checks each arrives whole and the
// server goes on serving.
func TestParallelUploads(t *testing.T) {
	t.Parallel()
	e, image := underTest(t)
	s := serve(t, e, image, spec{port: portParallelUploads})

	results := make([]uploaded, 3)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i] = s.upload(t.Context(), fmt.Sprintf("7-%d", i+1), "application/octet-stream")
		})
	}
	wg.Wait()
	for i, got := range results {
		checkUpload(t, s, fmt.Sprintf("7-%d", i+1), fmt.Sprintf("parallel upload %d of %d", i+1, len(results)), got)
	}
	statusIs(t, s, "/backend/getIP.php after the uploads", http.StatusOK, "/backend/getIP.php")
}

// checkUpload checks the upload tagged tag arrived whole. An upload the
// server stops reading half way still ends in a 200, so only the byte
// counts on both sides show it arrived whole.
func checkUpload(t *testing.T, s *served, tag, what string, got uploaded) {
	t.Helper()
	checks := harness.NewChecks(t)
	// The bash suite's name, which said what curl reported
	sent := what + ": curl exit, status, bytes sent"
	want := fmt.Sprintf("status %d, %d bytes sent", http.StatusOK, uploadBytes)
	if got.err != nil {
		checks.True(sent, false, fmt.Sprintf("%v, after %d bytes sent", got.err, got.sent))
	} else {
		harness.Equal(checks, sent, want, fmt.Sprintf("status %d, %d bytes sent", got.status, got.sent))
	}

	read := what + ": bytes the server read"
	n, err := bytesRead(t.Context(), s.c, uploadURI(tag))
	if err != nil {
		checks.True(read, false, err.Error())
		return
	}
	harness.Equal(checks, read, int64(uploadBytes), n)
}

// uploadURI is the request URI of the upload tagged tag, as the access log
// names it.
func uploadURI(tag string) string {
	return "/backend/empty.php?acc=" + tag
}

// upload posts uploadBytes of zeros to empty.php, tagged, with
// contentType, or none when it is empty. Browsers send no Expect:
// 100-continue, and neither does this.
func (s *served) upload(ctx context.Context, tag, contentType string) uploaded {
	body := &countingBody{r: io.LimitReader(zeros{}, uploadBytes), closed: make(chan struct{})}
	req, err := s.request(ctx, http.MethodPost, uploadURI(tag), body)
	if err != nil {
		return uploaded{err: err}
	}
	// With a body Go cannot measure, it would send it chunked, as no
	// browser sends an upload
	req.ContentLength = uploadBytes
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := harness.HostClient()
	client.Timeout = uploadWithin
	resp, err := send(client, req)
	// The client closes the body once it has written it, and may do so
	// after the answer is in
	select {
	case <-body.closed:
	case <-ctx.Done():
		err = ctx.Err()
	}
	return uploaded{status: resp.status, sent: body.n.Load(), err: err}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// countingBody is a request body that counts the bytes read from it, and
// says when it is closed: by then the client has sent all it ever will.
type countingBody struct {
	r      io.Reader
	n      atomic.Int64
	once   sync.Once
	closed chan struct{}
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}
