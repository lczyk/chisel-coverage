package cut

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/lczyk/assert"
)

// fakeChisel prints what $FAKE_SAYS holds and exits $FAKE_CODE after creating
// a file and a symlink in --root. With $FAKE_ARGS set it writes its
// arguments there, one a line.
const fakeChisel = `#!/bin/sh
if [ -n "$FAKE_ARGS" ]; then printf '%s\n' "$@" > "$FAKE_ARGS"; fi
while [ $# -gt 1 ]; do
  case "$1" in --root) root="$2"; shift 2;; *) shift;; esac
done
printf '%s\n' "$FAKE_SAYS" >&2
if [ "$FAKE_CODE" != 0 ]; then exit "$FAKE_CODE"; fi
mkdir -p "$root/usr/bin"
echo x > "$root/usr/bin/demo"
ln -s demo "$root/usr/bin/d"
exit 0
`

func fake(t *testing.T) *Runner {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "chisel")
	assert.NoError(t, os.WriteFile(bin, []byte(fakeChisel), 0755))
	return &Runner{Bin: bin}
}

func TestCutWalksAndReadsVersion(t *testing.T) {
	r := fake(t)
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_SAYS", "2026/01/01 00:00:00 Fetching pool/main/d/demo/demo_1.0-1ubuntu2_all.deb...")
	res, err := r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.NoError(t, err)
	assert.EqualArrays(t, res.Paths, []string{"/usr/", "/usr/bin/", "/usr/bin/d", "/usr/bin/demo"})
	assert.Equal(t, res.Version, "1.0-1ubuntu2")
	assert.Equal(t, res.DebArch, "all")
}

func TestCutWith(t *testing.T) {
	r := fake(t)
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_ARGS", args)
	t.Setenv("FAKE_SAYS", "Fetching pool/main/d/demo/demo_2.0_amd64.deb...")
	res, err := r.CutAs(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins", "demo_bins@base", "demo_extra", "other_libs")
	assert.NoError(t, err)
	assert.Equal(t, res.Version, "2.0")
	b, err := os.ReadFile(args)
	assert.NoError(t, err)
	assert.That(t, bytes.HasSuffix(b, []byte("demo_bins\ndemo_extra\nother_libs\n")))

	// A missing package is the slice's own only when it is.
	t.Setenv("FAKE_CODE", "1")
	t.Setenv("FAKE_SAYS", `error: cannot find package "other" in archive(s)`)
	_, err = r.CutAs(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins", "demo_bins", "other_libs")
	assert.ErrorIs(t, err, ErrEssentialNotAvailable)
}

func TestCutVersionIsThisPackagesOnly(t *testing.T) {
	r := fake(t)
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_SAYS", "Fetching pool/main/o/other/other_9_amd64.deb...\nFetching pool/main/d/demo/demo_2.0_amd64.deb...")
	res, err := r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.NoError(t, err)
	assert.Equal(t, res.Version, "2.0")
	assert.Equal(t, res.DebArch, "amd64")

	t.Setenv("FAKE_SAYS", "nothing fetched")
	res, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.NoError(t, err)
	assert.Equal(t, res.Version, "")
}

func TestCutClassifiesFailures(t *testing.T) {
	r := fake(t)
	t.Setenv("FAKE_CODE", "1")
	t.Setenv("FAKE_SAYS", `error: cannot find package "demo" in archive(s)`)
	_, err := r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.ErrorIs(t, err, ErrNotAvailable)

	assert.Equal(t, err.Error(), ErrNotAvailable.Error())
	var ce *Error
	assert.That(t, errors.As(err, &ce))
	assert.Equal(t, ce.Package, "demo")
	// Chisel's other wording, for a package one archive was asked for.
	t.Setenv("FAKE_SAYS", `error: cannot find package "demo" in archive`)
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.ErrorIs(t, err, ErrNotAvailable)

	// The same text about another package is not this package's absence.
	t.Setenv("FAKE_SAYS", `error: cannot find package "libc6" in archive(s)`)
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.That(t, !errors.Is(err, ErrNotAvailable))
	assert.ErrorIs(t, err, ErrEssentialNotAvailable)
	assert.That(t, errors.As(err, &ce))
	assert.Equal(t, ce.Package, "libc6")
	assert.Equal(t, ce.Args[1], "cut")
	assert.EqualArrays(t, ce.Args[len(ce.Args)-6:], []string{"amd64", "--ignore", "unmaintained", "--ignore", "unstable", "demo_bins"})
	assert.ContainsString(t, err.Error(), "not available for this architecture\n  error: cannot find package")

	t.Setenv("FAKE_SAYS", `error: cannot extract from package "demo": no content at /usr/share/man/**`)
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.ErrorIs(t, err, ErrNoContent)
	assert.That(t, errors.As(err, &ce))
	assert.ContainsString(t, err.Error(), "matched nothing\n  error: cannot extract")
	assert.Equal(t, ce.Package, "demo")
	assert.EqualArrays(t, ce.Paths, []string{"/usr/share/man/**"})

	// Several paths come as a list, and the package may be one the slice
	// reaches.
	t.Setenv("FAKE_SAYS", "error: cannot extract from package \"libc6\": no content at:\n- /a\n- /b/**\nnot a path")
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.ErrorIs(t, err, ErrNoContent)
	assert.That(t, errors.As(err, &ce))
	assert.Equal(t, ce.Package, "libc6")
	assert.EqualArrays(t, ce.Paths, []string{"/a", "/b/**"})

	// A hard link with no target names the target, which no slice asked for.
	t.Setenv("FAKE_SAYS", `error: cannot extract from package "demo": cannot create hard link /usr/bin/a: no content at /usr/bin/b`)
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.That(t, !errors.Is(err, ErrNoContent))
	assert.ContainsString(t, err.Error(), "exit status 1\n  error: cannot extract")

	// An archive chisel skipped is told with any failure, and once.
	t.Setenv("FAKE_SAYS", "Archive \"esm-apps\" ignored: credentials not found\nArchive \"esm-infra\" ignored: credentials not found\nArchive \"esm-apps\" ignored: credentials not found\nerror: cannot find package \"demo\" in archive(s)")
	_, err = r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.ErrorIs(t, err, ErrNotAvailable)
	assert.That(t, errors.As(err, &ce))
	assert.EqualArrays(t, ce.Skipped, []string{"esm-apps", "esm-infra"})
}

func TestCutReportsDebsAndSkippedArchives(t *testing.T) {
	r := fake(t)
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_SAYS", "Archive \"esm-apps\" ignored: credentials not found\nFetching pool/main/g/glibc/libc6_2.39-0ubuntu8_amd64.deb...\nFetching pool/main/d/demo/demo_2.0_all.deb...")
	res, err := r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "amd64", "demo_bins")
	assert.NoError(t, err)
	assert.Equal(t, res.Version, "2.0")
	assert.Equal(t, res.DebArch, "all")
	assert.EqualArrays(t, res.Debs, []Deb{{"libc6", "2.39-0ubuntu8", "amd64"}, {"demo", "2.0", "all"}})
	assert.EqualArrays(t, res.Skipped, []string{"esm-apps"})
}

func TestCutAsLabelsTheStreamedLines(t *testing.T) {
	r := fake(t)
	var out bytes.Buffer
	r.Verbose = &out
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_SAYS", "one")
	_, err := r.CutAs(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "arm64", "demo_bins", "demo_bins@base")
	assert.NoError(t, err)
	assert.Equal(t, stamped.ReplaceAllString(out.String(), ""), "arm64 demo_bins@base one\n")
}

// stamped matches the timestamp every logged line starts with.
var stamped = regexp.MustCompile(`(?m)^\d\d-\d\d-\d\dT\d\d:\d\d:\d\d `)

func TestFetch(t *testing.T) {
	r := fake(t)
	t.Setenv("FAKE_SAYS", "Fetching current ubuntu-24.04 release...")
	t.Setenv("FAKE_CODE", "0")
	var seen bytes.Buffer
	r.Verbose = &seen
	assert.NoError(t, r.Fetch(context.Background(), "ubuntu-24.04"))
	assert.ContainsString(t, seen.String(), " release ubuntu-24.04 Fetching current ubuntu-24.04 release...\n")

	t.Setenv("FAKE_SAYS", "error: cannot talk to release repository")
	t.Setenv("FAKE_CODE", "1")
	err := r.Fetch(context.Background(), "ubuntu-24.04")
	var e *Error
	assert.That(t, errors.As(err, &e), "%v", err)
	assert.EqualArrays(t, e.Args[1:], []string{"find", "--release", "ubuntu-24.04", "chisel-coverage-nothing"})
	assert.Error(t, err, "exit status 1\n  error: cannot talk to release repository")
}

func TestStamp(t *testing.T) {
	assert.That(t, stamped.MatchString(Stamp()+" "), Stamp())
}

func TestCutVerboseStreamsPrefixed(t *testing.T) {
	r := fake(t)
	var out bytes.Buffer
	r.Verbose = &out
	t.Setenv("FAKE_CODE", "0")
	t.Setenv("FAKE_SAYS", "2026/10/22 09:29:39 one\ntwo")
	_, err := r.Cut(context.Background(), "/rel", filepath.Join(t.TempDir(), "root"), "arm64", "demo_bins")
	assert.NoError(t, err)
	assert.Equal(t, stamped.ReplaceAllString(out.String(), ""), "arm64 demo_bins one\narm64 demo_bins two\n")
}

func TestPrefixWriterSplitsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	p := &prefixWriter{w: &out, prefix: "> "}
	_, _ = p.Write([]byte("ab"))
	_, _ = p.Write([]byte("c\nd\ne"))
	_, _ = p.Write([]byte("\n"))
	assert.Equal(t, stamped.ReplaceAllString(out.String(), ""), "> abc\n> d\n> e\n")
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	assert.NoError(t, os.MkdirAll(filepath.Join(root, "etc", "empty.d"), 0755))
	assert.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "a.so"), []byte("abc"), 0644))
	assert.NoError(t, os.Symlink("usr/lib", filepath.Join(root, "lib")))
	paths, bytes, err := Walk(root)
	assert.NoError(t, err)
	assert.EqualArrays(t, paths, []string{"/etc/", "/etc/empty.d/", "/lib", "/usr/", "/usr/lib/", "/usr/lib/a.so"})
	assert.Equal(t, bytes, int64(3))

	_, _, err = Walk(filepath.Join(root, "missing"))
	assert.Error(t, err, "no such file")
}

func TestBinary(t *testing.T) {
	t.Setenv("CHISEL", "")
	assert.Equal(t, Binary(), "chisel")
	t.Setenv("CHISEL", "/opt/chisel")
	assert.Equal(t, Binary(), "/opt/chisel")
}
