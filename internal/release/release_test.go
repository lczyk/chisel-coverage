package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
)

// chisel stands in for chisel fetching a release: it drops chisel.yaml where
// chisel would, or fails, and counts the calls.
type chisel struct {
	cache string
	fail  error
	calls int
	names []string
}

func (c *chisel) fetch(_ context.Context, name string) error {
	c.calls++
	c.names = append(c.names, name)
	if c.fail != nil {
		return c.fail
	}
	dir := filepath.Join(c.cache, "releases", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "chisel.yaml"), []byte("format: v1\n"), 0644)
}

func TestResolveDirectoryAsIs(t *testing.T) {
	c := &chisel{cache: t.TempDir()}
	dir := t.TempDir()
	got, err := Resolve(context.Background(), dir, c.cache, c.fetch, nil)
	assert.NoError(t, err)
	assert.Equal(t, got, dir)
	assert.Equal(t, c.calls, 0)
}

func TestResolveRejectsOther(t *testing.T) {
	c := &chisel{cache: t.TempDir()}
	for _, arg := range []string{filepath.Join(t.TempDir(), "missing"), "Ubuntu-24.04", "ubuntu", "u-24.04", "ubuntu-24.04/"} {
		_, err := Resolve(context.Background(), arg, c.cache, c.fetch, nil)
		assert.Error(t, err, "not a directory or a release name", arg)
	}
	assert.Equal(t, c.calls, 0)
	_, err := Resolve(context.Background(), "ubuntu-24.04", "", c.fetch, nil)
	assert.Error(t, err, "cannot find chisel's cache")
}

func TestResolveHasChiselFetch(t *testing.T) {
	c := &chisel{cache: t.TempDir()}
	var notices []string
	logf := func(format string, args ...any) { notices = append(notices, fmt.Sprintf(format, args...)) }
	for _, name := range []string{"ubuntu-24.04", "ubuntu-26.04", "ubuntu-devel-26.10"} {
		got, err := Resolve(context.Background(), name, c.cache, c.fetch, logf)
		assert.NoError(t, err)
		assert.Equal(t, got, filepath.Join(c.cache, "releases", name))
	}
	assert.EqualArrays(t, c.names, []string{"ubuntu-24.04", "ubuntu-26.04", "ubuntu-devel-26.10"})
	assert.Len(t, notices, 0)
}

func TestResolveOfflineUsesChiselsCopy(t *testing.T) {
	c := &chisel{cache: t.TempDir()}
	dir, err := Resolve(context.Background(), "ubuntu-24.04", c.cache, c.fetch, nil)
	assert.NoError(t, err)

	c.fail = errors.New("chisel find: exit status 1\n  error: cannot talk to release repository")
	var notices []string
	logf := func(format string, args ...any) { notices = append(notices, fmt.Sprintf(format, args...)) }
	got, err := Resolve(context.Background(), "ubuntu-24.04", c.cache, c.fetch, logf)
	assert.NoError(t, err)
	assert.Equal(t, got, dir)
	assert.EqualArrays(t, notices, []string{"release ubuntu-24.04: chisel cannot refresh it; using its cached copy\n  chisel find: exit status 1\n  error: cannot talk to release repository"})

	// Nothing cached: chisel's error stands.
	_, err = Resolve(context.Background(), "ubuntu-26.04", c.cache, c.fetch, logf)
	assert.Error(t, err, "cannot talk to release repository")

	// Interrupted: the copy is not an answer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.fail = context.Canceled
	_, err = Resolve(ctx, "ubuntu-24.04", c.cache, c.fetch, logf)
	assert.Error(t, err, "context canceled")
}

func TestResolveExpectsChiselsLayout(t *testing.T) {
	c := &chisel{cache: t.TempDir()}
	elsewhere := &chisel{cache: t.TempDir()}
	_, err := Resolve(context.Background(), "ubuntu-24.04", c.cache, elsewhere.fetch, nil)
	assert.Error(t, err, "chisel fetched release ubuntu-24.04 but left nothing at "+filepath.Join(c.cache, "releases", "ubuntu-24.04"))
}
