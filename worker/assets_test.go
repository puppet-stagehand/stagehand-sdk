package worker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

type chunk struct {
	data []byte
	ct   string
	last bool
}

func getAsset(t *testing.T, host *workertest.FakeHost, p string) ([]chunk, error) {
	t.Helper()
	stream, err := host.Assets().Get(context.Background(), &hostv1.AssetRequest{Path: p})
	if err != nil {
		return nil, err
	}
	var out []chunk
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, chunk{data: c.GetData(), ct: c.GetContentType(), last: c.GetLast()})
	}
}

func manifestFor(files map[string]string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"format":1,"entry":"index.js","slots":{},"files":{`)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	first := true
	for _, p := range paths {
		ct := files[p]
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `%q:{"sha256":"%064d","size":1,"content_type":%q}`, p, 0, ct)
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

func code(err error) codes.Code { return status.Code(err) }

func TestAssetsServeOnlyListedFilesAndTheManifest(t *testing.T) {
	ui := fstest.MapFS{
		"ui.manifest.json": {Data: manifestFor(map[string]string{"index.js": "text/javascript", "app.css": "text/css"})},
		"index.js":         {Data: []byte("js")},
		"app.css":          {Data: []byte("css")},
		"secret.txt":       {Data: []byte("not in the bundle")},
		"sub/hidden.js":    {Data: []byte("also unlisted")},
	}
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{UI: ui})

	got, err := getAsset(t, host, "ui.manifest.json")
	if err != nil || len(got) != 1 || !bytes.Equal(got[0].data, ui["ui.manifest.json"].Data) {
		t.Fatalf("manifest = %v, %v, want the exact bytes of ui.manifest.json", got, err)
	}
	if got[0].ct != "application/json" {
		t.Fatalf("manifest content type = %q, want application/json", got[0].ct)
	}
	got, err = getAsset(t, host, "app.css")
	if err != nil || string(got[0].data) != "css" || got[0].ct != "text/css" {
		t.Fatalf("app.css = %+v, %v (the content type comes from the manifest)", got, err)
	}
	for _, p := range []string{"secret.txt", "sub/hidden.js", "missing.js"} {
		if _, err := getAsset(t, host, p); code(err) != codes.NotFound {
			t.Errorf("Get(%q) = %v, want NotFound", p, err)
		}
	}
}

func TestAssetsRefuseUnsafePaths(t *testing.T) {
	ui := fstest.MapFS{
		"ui.manifest.json": {Data: manifestFor(map[string]string{"index.js": "text/javascript", "../evil.js": "text/javascript"})},
		"index.js":         {Data: []byte("js")},
	}
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{UI: ui})

	for _, p := range []string{
		"../evil.js", "a/../index.js", "/index.js", "/etc/passwd", "..", "a..b", ".", "./index.js",
		"index.js/", "a//b", "back\\slash", "nul\x00.js", "",
	} {
		if _, err := getAsset(t, host, p); code(err) != codes.InvalidArgument {
			t.Errorf("Get(%q) = %v, want InvalidArgument", p, err)
		}
	}
}

func TestAssetsChunking(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 600*1024/16) // 600 KiB
	ui := fstest.MapFS{
		"ui.manifest.json": {Data: manifestFor(map[string]string{"big.js": "text/javascript", "empty.json": "application/json", "exact.js": "text/javascript"})},
		"big.js":           {Data: big},
		"empty.json":       {Data: nil},
		"exact.js":         {Data: make([]byte, worker.MaxAssetChunk)},
	}
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{UI: ui})

	got, err := getAsset(t, host, "big.js")
	if err != nil {
		t.Fatalf("Get big.js: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("600 KiB arrived in %d chunks, want 3", len(got))
	}
	var all []byte
	for i, c := range got {
		if len(c.data) > worker.MaxAssetChunk {
			t.Errorf("chunk %d is %d bytes, over the %d cap", i, len(c.data), worker.MaxAssetChunk)
		}
		if c.last != (i == len(got)-1) {
			t.Errorf("chunk %d last = %v", i, c.last)
		}
		all = append(all, c.data...)
	}
	if !bytes.Equal(all, big) {
		t.Fatal("reassembled chunks differ from the file")
	}

	got, err = getAsset(t, host, "empty.json")
	if err != nil || len(got) != 1 || !got[0].last || len(got[0].data) != 0 {
		t.Fatalf("empty file = %+v, %v, want one empty last chunk", got, err)
	}
	got, err = getAsset(t, host, "exact.js")
	if err != nil || len(got) != 1 || !got[0].last {
		t.Fatalf("a file of exactly one chunk = %d chunks, %v, want 1 chunk marked last", len(got), err)
	}
}

func TestNoUIBundleMeansNoAssets(t *testing.T) {
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{UI: fstest.MapFS{}})

	if _, err := getAsset(t, host, "index.js"); code(err) != codes.NotFound {
		t.Fatalf("Get on a pack without a UI = %v, want NotFound", err)
	}
}

func TestUnparsableUIManifestStopsRun(t *testing.T) {
	host := workertest.NewFakeHost(t)
	for k, v := range host.Env() {
		t.Setenv(k, v)
	}
	ui := fstest.MapFS{"ui.manifest.json": {Data: []byte(`{"files": nope`)}}
	errc := make(chan error, 1)
	go func() { errc <- worker.Run(context.Background(), worker.Options{UI: ui}) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Run accepted an unparsable ui.manifest.json")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not fail on an unparsable ui.manifest.json")
	}
}
