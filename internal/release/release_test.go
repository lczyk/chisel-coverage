package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

// tarball is a github branch archive: every entry under one top-level dir.
func tarball(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	assert.NoError(t, tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0755}))
	for name, body := range files {
		assert.NoError(t, tw.WriteHeader(&tar.Header{Name: top + "/" + name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		assert.NoError(t, err)
	}
	assert.NoError(t, tw.Close())
	assert.NoError(t, gz.Close())
	return buf.Bytes()
}

// github serves one release tarball under an etag and counts requests.
type github struct {
	t        *testing.T
	body     []byte
	etag     string
	requests int
}

func (g *github) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.requests++
	if !strings.HasSuffix(r.URL.Path, "/ubuntu-24.04") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("If-None-Match") == g.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", g.etag)
	_, _ = w.Write(g.body)
}

func serve(t *testing.T, g *github) {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL + "/"
	t.Cleanup(func() { baseURL = old })
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	assert.NoError(t, err)
	return string(b)
}

func TestResolveDirectoryAsIs(t *testing.T) {
	dir := t.TempDir()
	got, err := Resolve(context.Background(), dir, t.TempDir(), nil)
	assert.NoError(t, err)
	assert.Equal(t, got, dir)
}

func TestResolveRejectsOther(t *testing.T) {
	_, err := Resolve(context.Background(), filepath.Join(t.TempDir(), "missing"), t.TempDir(), nil)
	assert.Error(t, err, "not a directory or a release name")
	_, err = Resolve(context.Background(), "Ubuntu-24.04", t.TempDir(), nil)
	assert.Error(t, err, "not a directory or a release name")
}

func TestResolveFetchesThenReusesThenRefreshes(t *testing.T) {
	g := &github{t: t, etag: "v1", body: tarball(t, "chisel-releases-ubuntu-24.04", map[string]string{"chisel.yaml": "format: v1\n", "slices/bash.yaml": "package: bash\n"})}
	serve(t, g)
	cache := t.TempDir()
	var notices []string
	logf := func(format string, args ...any) { notices = append(notices, fmt.Sprintf(format, args...)) }

	dir, err := Resolve(context.Background(), "ubuntu-24.04", cache, logf)
	assert.NoError(t, err)
	assert.Equal(t, dir, filepath.Join(cache, "releases", "ubuntu-24.04"))
	assert.Equal(t, read(t, filepath.Join(dir, "chisel.yaml")), "format: v1\n")
	assert.Equal(t, read(t, filepath.Join(dir, "slices", "bash.yaml")), "package: bash\n")
	assert.Equal(t, read(t, filepath.Join(dir, ".etag")), "v1")
	assert.EqualArrays(t, notices, []string{"release ubuntu-24.04: fetching from github"})

	// Up to date: github answers 304 and the directory is left alone.
	marker := filepath.Join(dir, "marker")
	assert.NoError(t, os.WriteFile(marker, nil, 0644))
	_, err = Resolve(context.Background(), "ubuntu-24.04", cache, logf)
	assert.NoError(t, err)
	assert.Equal(t, g.requests, 2)
	_, err = os.Stat(marker)
	assert.NoError(t, err)

	// A new branch head replaces the directory wholesale.
	g.etag = "v2"
	g.body = tarball(t, "chisel-releases-ubuntu-24.04", map[string]string{"chisel.yaml": "format: v2\n"})
	_, err = Resolve(context.Background(), "ubuntu-24.04", cache, logf)
	assert.NoError(t, err)
	assert.Equal(t, read(t, filepath.Join(dir, "chisel.yaml")), "format: v2\n")
	_, err = os.Stat(marker)
	assert.Error(t, err, "no such file")
	_, err = os.Stat(filepath.Join(dir, "slices"))
	assert.Error(t, err, "no such file")
}

func TestResolveUnknownRelease(t *testing.T) {
	serve(t, &github{t: t, etag: "v1", body: tarball(t, "x", nil)})
	_, err := Resolve(context.Background(), "ubuntu-99.99", t.TempDir(), nil)
	assert.Error(t, err, "no such release ubuntu-99.99")
}

func TestResolveOfflineUsesCache(t *testing.T) {
	g := &github{t: t, etag: "v1", body: tarball(t, "top", map[string]string{"chisel.yaml": "format: v1\n"})}
	srv := httptest.NewServer(g)
	old := baseURL
	baseURL = srv.URL + "/"
	t.Cleanup(func() { baseURL = old })
	cache := t.TempDir()
	dir, err := Resolve(context.Background(), "ubuntu-24.04", cache, nil)
	assert.NoError(t, err)
	srv.Close()

	var notices []string
	logf := func(format string, args ...any) { notices = append(notices, fmt.Sprintf(format, args...)) }
	got, err := Resolve(context.Background(), "ubuntu-24.04", cache, logf)
	assert.NoError(t, err)
	assert.Equal(t, got, dir)
	assert.Len(t, notices, 1)
	assert.That(t, strings.HasPrefix(notices[0], "release ubuntu-24.04: cannot reach github ("), notices[0])
	assert.That(t, strings.HasSuffix(notices[0], "); using the cached copy"), notices[0])

	// Nothing cached: the error stands.
	_, err = Resolve(context.Background(), "ubuntu-24.04", t.TempDir(), nil)
	assert.Error(t, err, "cannot fetch release ubuntu-24.04")
}

func TestExtractRejectsEscapes(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	assert.NoError(t, tw.WriteHeader(&tar.Header{Name: "top/../../evil", Typeflag: tar.TypeReg, Mode: 0644, Size: 0}))
	assert.NoError(t, tw.Close())
	assert.NoError(t, gz.Close())
	err := extract(&buf, t.TempDir())
	assert.Error(t, err, "escapes the release")
}
