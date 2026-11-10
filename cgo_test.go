package rdma_test

import (
	"go/build"
	"testing"
)

// TestNoCgoFiles checks that this package and rsocket build on Linux without
// cgo: neither has a file importing "C".
func TestNoCgoFiles(t *testing.T) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "linux", "amd64", true
	for _, dir := range []string{".", "rsocket"} {
		pkg, err := ctx.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("ImportDir(%q) error: %v", dir, err)
		}
		if len(pkg.CgoFiles) != 0 {
			t.Errorf("package %s has cgo files %v, want none", pkg.Name, pkg.CgoFiles)
		}
	}
}
