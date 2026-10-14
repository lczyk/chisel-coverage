package coverage

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/chisel-proxy/pkg/apt"
	"github.com/lczyk/chisel-proxy/pkg/deb"

	"github.com/lczyk/chisel-slice-coverage/internal/cut"
)

// TestRealChisel pins what the design assumes about chisel: that the whole-deb
// glob, the real slices and the ignore globs coexist in one package, that a
// slice's rootfs carries the implicit parents of its files, that an ignore
// glob matching nothing ignores nothing, and that a package absent from the
// archive is reported as such. It runs the real chisel
// against an in-memory signed archive reached through http_proxy, with a
// throwaway cache. Skipped without a chisel binary.
func TestRealChisel(t *testing.T) {
	bin := cut.Binary()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("chisel binary not found (%v); set CHISEL=/path/to/chisel", err)
	}
	arch, err := deb.InferArch()
	assert.NoError(t, err)

	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "demo")
	for _, p := range []string{"usr/bin/demo", "usr/lib/demo/plugin.so", "usr/share/doc/demo/README", "usr/share/doc/demo/copyright"} {
		assert.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(pkgDir, p)), 0755))
		assert.NoError(t, os.WriteFile(filepath.Join(pkgDir, p), []byte(p), 0644))
	}
	pkg, err := deb.FromDir(pkgDir, deb.Defaults{Arch: arch, Version: "1.0-1"})
	assert.NoError(t, err)
	archive, err := apt.New("cov", arch, []*deb.Package{pkg})
	assert.NoError(t, err)

	// chisel only knows archive.ubuntu.com and ports.ubuntu.com, and reaches
	// them through http_proxy; the proxy answers for the suite from memory.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ctype, ok := archive.Serve(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))

	armor, err := archive.PublicKey()
	assert.NoError(t, err)
	var y strings.Builder
	fmt.Fprintf(&y, "format: v1\n\nmaintenance:\n  standard: 2024-04-25\n  expanded: 2029-05-31\n  legacy: 2034-04-25\n  end-of-life: 2036-04-29\n\narchives:\n  cov:\n    version: 24.04\n    components: [cov]\n    suites: [cov]\n    priority: 10\n    public-keys: [cov-key]\n\n")
	fmt.Fprintf(&y, "public-keys:\n  cov-key:\n    id: %q\n    armor: |\n", archive.KeyID())
	for _, line := range strings.Split(strings.TrimRight(armor, "\n"), "\n") {
		fmt.Fprintf(&y, "      %s\n", line)
	}
	release := filepath.Join(tmp, "release")
	assert.NoError(t, os.MkdirAll(filepath.Join(release, "slices"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(release, "chisel.yaml"), []byte(y.String()), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(release, "slices", "demo.yaml"), []byte(`package: demo

essential:
  - demo_copyright

slices:
  bins:
    essential:
      - demo_copyright
    contents:
      /usr/bin/demo:
      /usr/bin/sh: {symlink: /usr/bin/demo}
  copyright:
    contents:
      /usr/share/doc/demo/copyright:
  scripts:
    mutate: |
      content.write("/usr/bin/demo", "")
`), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(release, "slices", "ghost.yaml"), []byte("package: ghost\nslices:\n  all:\n    contents:\n      /x:\n"), 0644))

	res, err := Run(Options{
		Release: release,
		Arches:  []string{arch},
		Ignores: []string{"/usr/share/doc/**", "/usr/share/man/**"},
		Runner:  &cut.Runner{Bin: bin},
	}, []string{"demo", "ghost"})
	assert.NoError(t, err)
	assert.Len(t, res, 2)

	demo := res[0].Arches[0]
	assert.Nil(t, demo.Err)
	assert.That(t, demo.Available)
	assert.Equal(t, demo.Version, "1.0-1")
	assert.Equal(t, demo.DebArch, arch)
	assert.EqualArrays(t, res[0].Slices, []string{"bins", "copyright", "scripts"})
	assert.EqualArrays(t, demo.Slices["bins"], []string{"/usr/", "/usr/bin/", "/usr/bin/demo"})
	assert.EqualArrays(t, demo.Slices["copyright"], []string{"/usr/", "/usr/share/", "/usr/share/doc/", "/usr/share/doc/demo/", "/usr/share/doc/demo/copyright"})
	assert.EqualArrays(t, demo.Slices["scripts"], []string{})
	assert.EqualArrays(t, demo.Uncovered, []string{"/usr/lib/", "/usr/lib/demo/", "/usr/lib/demo/plugin.so"})
	assert.EqualArrays(t, demo.Ignored, []string{"/usr/share/doc/demo/README"})
	assert.Equal(t, demo.Total, len(demo.Covered)+len(demo.Uncovered)+len(demo.Ignored))

	ghost := res[1].Arches[0]
	assert.Nil(t, ghost.Err)
	assert.That(t, !ghost.Available)
}
