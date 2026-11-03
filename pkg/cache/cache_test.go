package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	assert.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	assert.NoError(t, os.WriteFile(p, []byte(content), 0644))
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	assert.NoError(t, err)
	fb, err := os.Stat(b)
	assert.NoError(t, err)
	return os.SameFile(fa, fb)
}

func TestDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/x")
	t.Setenv("HOME", "/h")
	assert.Equal(t, Dir(), "/x/chisel")
	t.Setenv("XDG_CACHE_HOME", "")
	assert.Equal(t, Dir(), "/h/.cache/chisel")
	t.Setenv("HOME", "")
	assert.Equal(t, Dir(), "")
}

func TestViewSeedsMergesAndRemoves(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "chisel")
	write(t, filepath.Join(shared, "sha256", "aa"), "a")
	write(t, filepath.Join(shared, "sha512", "bb"), "b")
	write(t, filepath.Join(shared, "sha256", "cc.tmp"), "half")
	write(t, filepath.Join(shared, "releases", ".lock"), "")

	v, err := New(shared)
	assert.NoError(t, err)
	assert.Equal(t, filepath.Dir(filepath.Dir(v.Home)), filepath.Dir(shared))
	view := filepath.Join(v.Home, "chisel")
	assert.That(t, sameFile(t, filepath.Join(shared, "sha256", "aa"), filepath.Join(view, "sha256", "aa")))
	assert.That(t, sameFile(t, filepath.Join(shared, "sha512", "bb"), filepath.Join(view, "sha512", "bb")))
	_, err = os.Stat(filepath.Join(view, "sha256", "cc.tmp"))
	assert.That(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(view, "releases"))
	assert.That(t, os.IsNotExist(err))

	// chisel fetched dd into the view, left a temp file, and someone else
	// published ee in the shared cache meanwhile.
	write(t, filepath.Join(view, "sha256", "dd"), "d")
	write(t, filepath.Join(view, "sha256", "ff.tmp"), "half")
	write(t, filepath.Join(shared, "sha256", "ee"), "e")
	assert.NoError(t, v.Merge())
	assert.That(t, sameFile(t, filepath.Join(shared, "sha256", "dd"), filepath.Join(view, "sha256", "dd")))
	_, err = os.Stat(filepath.Join(shared, "sha256", "ff.tmp"))
	assert.That(t, os.IsNotExist(err))
	// Merging again is a no-op, and a blob both sides have is left alone.
	write(t, filepath.Join(view, "sha256", "ee"), "e2")
	assert.NoError(t, v.Merge())
	b, err := os.ReadFile(filepath.Join(shared, "sha256", "ee"))
	assert.NoError(t, err)
	assert.Equal(t, string(b), "e")

	assert.NoError(t, v.Remove())
	_, err = os.Stat(v.Home)
	assert.That(t, os.IsNotExist(err))
	b, err = os.ReadFile(filepath.Join(shared, "sha256", "dd"))
	assert.NoError(t, err)
	assert.Equal(t, string(b), "d")
}

func TestViewOfEmptyCache(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "chisel")
	v, err := New(shared)
	assert.NoError(t, err)
	write(t, filepath.Join(v.Home, "chisel", "sha256", "aa"), "a")
	assert.NoError(t, v.Merge())
	_, err = os.Stat(filepath.Join(shared, "sha256", "aa"))
	assert.NoError(t, err)
	assert.NoError(t, v.Remove())
}

func TestNewSweepsStaleViews(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "chisel")
	base := filepath.Join(filepath.Dir(shared), "chisel-coverage")
	stale := filepath.Join(base, "view-stale")
	fresh := filepath.Join(base, "view-fresh")
	other := filepath.Join(base, "other")
	for _, d := range []string{stale, fresh, other} {
		assert.NoError(t, os.MkdirAll(d, 0755))
	}
	old := time.Now().Add(-48 * time.Hour)
	assert.NoError(t, os.Chtimes(stale, old, old))
	assert.NoError(t, os.Chtimes(other, old, old))

	v, err := New(shared)
	assert.NoError(t, err)
	defer v.Remove()
	_, err = os.Stat(stale)
	assert.That(t, os.IsNotExist(err))
	_, err = os.Stat(fresh)
	assert.NoError(t, err)
	_, err = os.Stat(other)
	assert.NoError(t, err)
}

func TestCopyMerge(t *testing.T) {
	// The copying fallback, as used when the shared cache is on another
	// filesystem, is exercised directly since a test cannot cross one.
	shared := filepath.Join(t.TempDir(), "chisel")
	v, err := New(shared)
	assert.NoError(t, err)
	defer v.Remove()
	v.copy = true
	write(t, filepath.Join(v.Home, "chisel", "sha256", "aa"), "a")
	assert.NoError(t, v.Merge())
	b, err := os.ReadFile(filepath.Join(shared, "sha256", "aa"))
	assert.NoError(t, err)
	assert.Equal(t, string(b), "a")
	assert.That(t, !sameFile(t, filepath.Join(shared, "sha256", "aa"), filepath.Join(v.Home, "chisel", "sha256", "aa")))
	entries, err := os.ReadDir(filepath.Join(shared, "sha256"))
	assert.NoError(t, err)
	assert.Len(t, entries, 1)
}
