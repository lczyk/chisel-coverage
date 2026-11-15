package suitecache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lczyk/assert"
)

// get fetches u through the proxy.
func get(t *testing.T, p *Proxy, u string) (int, string) {
	t.Helper()
	pu, err := url.Parse(p.URL)
	assert.NoError(t, err)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get(u)
	assert.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	return resp.StatusCode, string(b)
}

func TestProxy(t *testing.T) {
	var hits sync.Map
	count := func(path string) int {
		v, _ := hits.LoadOrStore(path, new(atomic.Int64))
		return int(v.(*atomic.Int64).Load())
	}
	fail := atomic.Bool{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int64))
		n := v.(*atomic.Int64).Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Hit", string(rune('0'+n)))
		_, _ = io.WriteString(w, r.URL.Path+" body")
	}))
	defer up.Close()
	p, err := Start()
	assert.NoError(t, err)
	defer p.Close()

	// An InRelease is fetched once, however many ask and at once.
	in := up.URL + "/ubuntu/dists/noble/InRelease"
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := get(t, p, in)
			assert.Equal(t, code, 200)
			assert.Equal(t, body, "/ubuntu/dists/noble/InRelease body")
		}()
	}
	wg.Wait()
	assert.Equal(t, count("/ubuntu/dists/noble/InRelease"), 1)
	fetched, served := p.Counts()
	assert.Equal(t, fetched, 1)
	assert.Equal(t, served, 7)

	// Another suite is another entry, and anything else is passed through
	// every time.
	get(t, p, up.URL+"/ubuntu/dists/noble-updates/InRelease")
	assert.Equal(t, count("/ubuntu/dists/noble-updates/InRelease"), 1)
	for range 3 {
		_, body := get(t, p, up.URL+"/ubuntu/pool/main/b/bash/bash.deb")
		assert.Equal(t, body, "/ubuntu/pool/main/b/bash/bash.deb body")
	}
	assert.Equal(t, count("/ubuntu/pool/main/b/bash/bash.deb"), 3)

	// A failure is the archive's own answer and is not remembered.
	fail.Store(true)
	bad := up.URL + "/ubuntu/dists/noble-security/InRelease"
	code, _ := get(t, p, bad)
	assert.Equal(t, code, http.StatusServiceUnavailable)
	fail.Store(false)
	code, body := get(t, p, bad)
	assert.Equal(t, code, 200)
	assert.Equal(t, body, "/ubuntu/dists/noble-security/InRelease body")
	get(t, p, bad)
	// Failed once (asked twice: the entry's try and the pass through),
	// then fetched once and served from memory.
	assert.Equal(t, count("/ubuntu/dists/noble-security/InRelease"), 3)
}
