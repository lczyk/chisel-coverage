package cache

import (
	"os"
	"os/exec"
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

// A view seeded with the recent blobs alone lacks the old ones, counts the
// ones chisel fetches again, and is made whole by Complete.
func TestRecent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "chisel")
	dir := filepath.Join(shared, "sha256")
	assert.NoError(t, os.MkdirAll(dir, 0755))
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"aa", "bb", "cc", "dd"} {
		assert.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0644))
	}
	for _, name := range []string{"bb", "cc", "dd"} {
		assert.NoError(t, os.Chtimes(filepath.Join(dir, name), old, old))
	}
	// A view of the recent blobs costs less to make, and a kept one to
	// take less still.
	recent, whole := CostRecent(shared, time.Hour), Cost(shared)
	assert.That(t, recent < whole, "cost")
	v, err := NewRecent(shared, time.Hour)
	assert.NoError(t, err)
	defer v.Remove()
	if keeps {
		assert.That(t, Cost(shared) < whole, "cost of a kept view")
	}
	view := filepath.Join(v.Home, "chisel", "sha256")
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(view, name))
		return err == nil
	}
	assert.That(t, has("aa") && !has("bb") && !has("cc") && !has("dd"), "seeded")
	assert.That(t, v.Partial(), "partial")
	assert.Equal(t, v.Blobs(), 1)

	// chisel fetches one the shared cache has, and one it does not.
	assert.NoError(t, os.WriteFile(filepath.Join(view, "bb"), []byte("bb"), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(view, "ee"), []byte("ee"), 0644))
	assert.NoError(t, v.Merge())
	assert.Equal(t, v.Refetched(), 1)
	b, err := os.ReadFile(filepath.Join(dir, "ee"))
	assert.NoError(t, err)
	assert.Equal(t, string(b), "ee")

	// One expired from the shared cache meanwhile is done without.
	assert.NoError(t, os.Remove(filepath.Join(dir, "dd")))
	assert.NoError(t, v.Complete())
	assert.That(t, has("aa") && has("bb") && has("cc") && has("ee") && !has("dd"), "whole")
	assert.That(t, !v.Partial(), "whole")
	assert.NoError(t, v.Merge())
	assert.Equal(t, v.Refetched(), 1)

	// A window of zero is every blob, as New.
	w, err := NewRecent(shared, 0)
	assert.NoError(t, err)
	defer w.Remove()
	assert.That(t, !w.Partial(), "whole from the start")
	assert.Equal(t, w.Blobs(), 4)
}

func keptViews(t *testing.T, shared string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(shared), "chisel-coverage"))
	assert.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// A view let go is the next one taken, brought up to date: it gains what
// the shared cache gained within the window, keeps what it had, and loses
// what the shared cache lost and what a chisel left half written.
func TestKeptViewIsTakenAgain(t *testing.T) {
	if !keeps {
		t.Skip("views are not kept here")
	}
	shared := filepath.Join(t.TempDir(), "chisel")
	dir := filepath.Join(shared, "sha256")
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"aa", "bb", "a0"} {
		write(t, filepath.Join(dir, name), name)
	}
	assert.NoError(t, os.Chtimes(filepath.Join(dir, "a0"), old, old))
	v, err := NewRecent(shared, time.Hour)
	assert.NoError(t, err)
	home := v.Home
	view := filepath.Join(home, "chisel", "sha256")
	assert.Equal(t, v.Blobs(), 2)
	assert.That(t, v.Partial())
	// A blob fetched and never merged, and a fetch cut short.
	write(t, filepath.Join(view, "f1"), "x")
	write(t, filepath.Join(view, "f2.tmp"), "half")
	assert.NoError(t, v.Release())
	assert.Nil(t, v.Lock())
	assert.Len(t, keptViews(t, shared), 1)

	// Meanwhile the shared cache gains two blobs, one of them old, and
	// loses one.
	write(t, filepath.Join(dir, "cc"), "cc")
	write(t, filepath.Join(dir, "b0"), "b0")
	assert.NoError(t, os.Chtimes(filepath.Join(dir, "b0"), old, old))
	assert.NoError(t, os.Remove(filepath.Join(dir, "bb")))
	// The view goes stale-looking but for the touch of its release.
	v, err = NewRecent(shared, time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, v.Home, home)
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(view, name))
		return err == nil
	}
	assert.That(t, has("aa") && has("cc"))
	assert.That(t, !has("bb") && !has("f1") && !has("f2.tmp") && !has("a0") && !has("b0"))
	assert.That(t, sameFile(t, filepath.Join(dir, "cc"), filepath.Join(view, "cc")))
	assert.Equal(t, v.Blobs(), 2)
	assert.That(t, v.Partial())
	assert.NoError(t, v.Complete())
	assert.That(t, has("a0") && has("b0") && !v.Partial())
	assert.NoError(t, v.Release())

	// Made whole, it is whole when taken again, and only gains.
	write(t, filepath.Join(dir, "dd"), "dd")
	v, err = NewRecent(shared, time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, v.Home, home)
	assert.That(t, !v.Partial() && has("dd") && has("a0"))
	assert.Equal(t, v.Blobs(), 5)
	assert.NoError(t, v.Remove())
	assert.Len(t, keptViews(t, shared), 0)
}

// A view a run has is no other run's, nor a sweep's, however stale; one let
// go and stale is swept.
func TestKeptViewIsOneRunsAtATime(t *testing.T) {
	if !keeps {
		t.Skip("views are not kept here")
	}
	shared := filepath.Join(t.TempDir(), "chisel")
	write(t, filepath.Join(shared, "sha256", "aa"), "a")
	a, err := New(shared)
	assert.NoError(t, err)
	b, err := New(shared)
	assert.NoError(t, err)
	assert.That(t, a.Home != b.Home)
	assert.Len(t, keptViews(t, shared), 2)

	old := time.Now().Add(-48 * time.Hour)
	assert.NoError(t, os.Chtimes(a.Home, old, old))
	assert.NoError(t, os.Chtimes(b.Home, old, old))
	sweep(filepath.Dir(a.Home))
	assert.Len(t, keptViews(t, shared), 2)

	// b is let go and goes stale; a is still taken.
	assert.NoError(t, b.Release())
	assert.NoError(t, os.Chtimes(b.Home, old, old))
	assert.NoError(t, os.Chtimes(a.Home, old, old))
	sweep(filepath.Dir(a.Home))
	left := keptViews(t, shared)
	assert.Len(t, left, 1)
	assert.Equal(t, left[0], filepath.Base(a.Home))
	_, err = os.Stat(filepath.Join(a.Home, "chisel", "sha256", "aa"))
	assert.NoError(t, err)
	assert.NoError(t, a.Remove())
}

// A lock got on a view swept since its lock file was opened is no lock on
// a view (kept.fizz, the design "noverify").
func TestLockOnASweptViewIsNoLock(t *testing.T) {
	if !keeps {
		t.Skip("views are not kept here")
	}
	base := t.TempDir()
	home := filepath.Join(base, "kept-x")
	write(t, filepath.Join(home, lockName), "")
	lock := lockKept(home)
	assert.That(t, lock != nil)
	// Swept by its holder: renamed away, and a view of the same name made.
	assert.NoError(t, os.Rename(home, filepath.Join(base, "trash-x")))
	assert.NoError(t, lock.Close())
	assert.Nil(t, lockKept(home))
	write(t, filepath.Join(home, lockName), "")
	stale, err := os.OpenFile(filepath.Join(base, "trash-x", lockName), os.O_RDWR, 0)
	assert.NoError(t, err)
	defer stale.Close()
	assert.That(t, tryLock(stale), "the old file locks, and is not the view's")
	again := lockKept(home)
	assert.That(t, again != nil, "the new view's own lock is free")
	assert.NoError(t, again.Close())
}

// A process that inherited the lock holds the view after the run let it go:
// the chisel a dead run left behind.
func TestInheritedLockHoldsTheView(t *testing.T) {
	if !keeps {
		t.Skip("views are not kept here")
	}
	shared := filepath.Join(t.TempDir(), "chisel")
	v, err := New(shared)
	assert.NoError(t, err)
	home := v.Home
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{v.Lock()}
	assert.NoError(t, cmd.Start())
	assert.NoError(t, v.Release())
	other, err := New(shared)
	assert.NoError(t, err)
	assert.That(t, other.Home != home, "taken while the child lives")
	assert.NoError(t, other.Remove())
	assert.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	again, err := New(shared)
	assert.NoError(t, err)
	assert.Equal(t, again.Home, home)
	assert.NoError(t, again.Remove())
}
