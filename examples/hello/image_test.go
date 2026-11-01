package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// imageCommand bounds every external operation, including cleanup. Explicitly
// requested image proof fails on missing Docker; it never silently skips.
func imageCommand(t *testing.T, dir string, env []string, timeout time.Duration, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func TestHelloImage(t *testing.T) {
	if os.Getenv("SDK_HELLO_IMAGE") != "1" {
		t.Skip("set SDK_HELLO_IMAGE=1 for production image proof")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	imageCommand(t, root, nil, 30*time.Second, "docker", "version")
	tag := fmt.Sprintf("stagehand-hello-proof:%d", time.Now().UnixNano())
	proof := tag + "-harness"
	container := fmt.Sprintf("stagehand-hello-proof-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", container).Run()
		_ = exec.CommandContext(ctx, "docker", "image", "rm", "-f", proof, tag).Run()
	})
	t.Log("building production linux/amd64 image")
	imageCommand(t, root, nil, 5*time.Minute, "docker", "buildx", "build", "--load", "--platform", "linux/amd64", "--provenance=false", "--sbom=false", "-f", "examples/hello/Dockerfile", "-t", tag, ".")
	dir := t.TempDir()
	archive := filepath.Join(dir, "image.tar")
	imageCommand(t, root, nil, time.Minute, "docker", "save", "-o", archive, tag)
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	files := readImageTar(t, f)
	var entries []struct {
		Config string
		Layers []string
	}
	if err := json.Unmarshal(files["manifest.json"], &entries); err != nil || len(entries) != 1 {
		t.Fatalf("docker-save manifest: %v, entries %d", err, len(entries))
	}
	var config struct {
		Architecture string
		OS           string
		Config       struct {
			Entrypoint []string
			User       string
		}
	}
	if err := json.Unmarshal(files[entries[0].Config], &config); err != nil {
		t.Fatal(err)
	}
	if config.OS != "linux" || config.Architecture != "amd64" || !reflect.DeepEqual(config.Config.Entrypoint, []string{"/hello"}) || (config.Config.User != "65532" && config.Config.User != "65532:65532") {
		t.Fatalf("unexpected production config: %+v", config)
	}
	layers := entries[0].Layers
	if len(layers) == 0 {
		t.Fatal("image has no filesystem layers")
	}
	wantManifest, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	wantAPI, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var topManifest, imageAPI []byte
	executable := false
	for i, layer := range layers {
		tr := layerReader(t, files[layer])
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimPrefix(h.Name, "./")
			if name == "hello" {
				executable = h.Mode&0111 != 0 && h.Typeflag == tar.TypeReg
			}
			if strings.HasPrefix(name, "stagehand/ui/") {
				t.Fatalf("headless image unexpectedly includes UI asset %s", name)
			}
			if name == "stagehand/manifest.json" && i == len(layers)-1 {
				topManifest, err = io.ReadAll(tr)
			}
			if name == "stagehand/openapi.json" {
				imageAPI, err = io.ReadAll(tr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !bytes.Equal(topManifest, wantManifest) {
		t.Fatal("topmost layer must contain the exact source manifest")
	}
	if !bytes.Equal(imageAPI, wantAPI) || !executable {
		t.Fatal("image must contain matching OpenAPI and executable /hello")
	}
	var m manifest.Manifest
	if err := json.Unmarshal(topManifest, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Slots) != 0 || m.UIDigest != "" || len(m.Jobs) != 0 || !reflect.DeepEqual(m.Permissions, []string{"documents:rw"}) {
		t.Fatal("image declares capabilities its headless worker cannot satisfy")
	}
	var api struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(imageAPI, &api); err != nil {
		t.Fatal(err)
	}
	if len(m.Routes) != 1 || m.Routes[0].Method != "GET" || m.Routes[0].Path != "greeting" || m.Routes[0].OperationID != api.Paths["/greeting"]["get"].OperationID {
		t.Fatal("image OpenAPI does not match its sole greeting route")
	}
	// The test executable is added only to a disposable derivative. The
	// production image's inherited /hello is the worker being exercised.
	imageCommand(t, root, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, time.Minute, "go", "test", "-c", "-o", filepath.Join(dir, "harness"), "./examples/hello")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+tag+"\nCOPY --chmod=0555 harness /harness\nENTRYPOINT [\"/harness\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	imageCommand(t, root, nil, time.Minute, "docker", "buildx", "build", "--load", "--platform", "linux/amd64", "--provenance=false", "--sbom=false", "-t", proof, dir)
	out := imageCommand(t, root, nil, time.Minute, "docker", "run", "--name", container, "--platform", "linux/amd64", "--network", "none", "-e", "SDK_HELLO_IMAGE_WORKER=1", proof, "-test.run=^TestImageWorkerHarness$", "-test.v", "-test.timeout=45s")
	if !bytes.Contains(out, []byte("HELLO_IMAGE_WORKER_OK")) || bytes.Contains(out, []byte("SKIP")) {
		t.Fatalf("missing real image-worker proof: %s", out)
	}
	t.Logf("%s", out)
	t.Log("HELLO_IMAGE_OK")
}

func readImageTar(t *testing.T, r io.Reader) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > 128<<20 {
			t.Fatalf("unexpectedly large image member %s", h.Name)
		}
		files[h.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func layerReader(t *testing.T, data []byte) *tar.Reader {
	t.Helper()
	var r io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gz.Close() })
		r = gz
	}
	return tar.NewReader(r)
}

// This test runs inside the derivative image, with FakeHost and the inherited
// production worker in the same container. No host networking is needed.
func TestImageWorkerHarness(t *testing.T) {
	if os.Getenv("SDK_HELLO_IMAGE_WORKER") != "1" {
		t.Skip("runs only in the disposable image harness")
	}
	for _, text := range []string{"", "Howdy"} {
		t.Run("existing="+text, func(t *testing.T) {
			docs := &fakeDocuments{docs: map[string]*structpb.Struct{}}
			if text != "" {
				body, err := structpb.NewStruct(map[string]any{"text": text})
				if err != nil {
					t.Fatal(err)
				}
				docs.docs["greetings/default"] = body
			}
			host := workertest.NewFakeHost(t, func(s grpc.ServiceRegistrar) { hostv1.RegisterDocumentsServer(s, docs) })
			cmd := exec.Command("/hello")
			cmd.Env = os.Environ()
			for k, v := range host.Env() {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			// Do not capture worker logs: transport environment contains ephemeral
			// test certificates and must never be dumped on a failure.
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				_ = cmd.Process.Signal(os.Interrupt)
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("worker shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					<-done
					t.Error("worker failed to stop cleanly")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := host.WaitConnected(ctx); err != nil {
				t.Fatal(err)
			}
			for {
				health, err := host.Health().Check(ctx, &emptypb.Empty{})
				if err == nil && health.GetReady() {
					break
				}
				if ctx.Err() != nil {
					t.Fatalf("worker readiness: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			want := text
			if want == "" {
				want = "Hello"
			}
			for _, principal := range []*hostv1.Principal{nil, {Label: "ada"}} {
				resp, got := dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "greeting", Principal: principal, Headers: map[string]string{"X-User": "root", "X-Stagehand-User": "root", "X-Principal": "root", "X-Principal-Label": "root"}})
				caller := "unknown"
				if principal != nil {
					caller = "ada"
				}
				if resp.GetStatus() != 200 || got.Greeting != want || got.Caller != caller {
					t.Fatalf("image greeting status=%d body=%+v", resp.GetStatus(), got)
				}
			}
			for _, p := range []string{"ui.manifest.json", "index.js"} {
				stream, err := host.Assets().Get(ctx, &hostv1.AssetRequest{Path: p})
				if err == nil {
					_, err = stream.Recv()
				}
				if status.Code(err) != codes.NotFound {
					t.Fatalf("asset %s: %v", p, err)
				}
			}
			for _, req := range []*hostv1.HttpRequest{{Method: "POST", Path: "greeting"}, {Method: "GET", Path: "elsewhere"}} {
				resp, _ := dispatch(t, host, req)
				if resp.GetStatus() == 200 {
					t.Fatalf("undeclared route served: %v", req)
				}
			}
			health, err := host.Health().Check(ctx, &emptypb.Empty{})
			if err != nil || !health.GetReady() {
				t.Fatalf("readiness after refusals: %v %v", health, err)
			}
		})
	}
	t.Log("HELLO_IMAGE_WORKER_OK")
}
