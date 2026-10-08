package build

import (
	"os"
	"path/filepath"
	"testing"
)

// The hello bundle that uibundle's tests use must be exactly what the builder
// produces from testdata/hello-src.
func TestGoldenHelloBundle(t *testing.T) {
	o, _ := work(t, "hello-src")
	if _, fs, err := Run(o); err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	golden := filepath.Join("..", "..", "..", "..", "uibundle", "testdata", "hello", "ui")
	for _, n := range []string{"index.js", "ui.manifest.json"} {
		got, _ := os.ReadFile(filepath.Join(o.Out, n))
		want, err := os.ReadFile(filepath.Join(golden, n))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the golden fixture\n--- got\n%s\n--- want\n%s", n, got, want)
		}
	}
}
