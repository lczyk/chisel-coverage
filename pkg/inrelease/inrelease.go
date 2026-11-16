// Package inrelease stops a run of many chisel cuts from fetching the same
// InRelease files over and over.
//
// The problem: chisel keeps packages and their indexes in its cache, but it
// asks the archive for every suite's InRelease again on each cut. For a
// small cut that is most of the time it takes.
//
// The fix: an HTTP proxy that the chisels of one run are pointed at with
// http_proxy. The first time an InRelease is asked for, the proxy fetches
// it and keeps it in memory. After that it answers from memory. Every
// other request is passed straight through to the archive.
//
// What this changes: all the cuts of a run see the archive as it was when
// the run first asked. If the archive is published again in the middle of
// a run, a cut that needs an index chisel has not cached yet gets the new
// index, chisel finds it does not match the InRelease it was given, and
// that cut fails with an error. Without the proxy the later cuts would
// have quietly used the new state.
//
// What it does not touch: archives reached over https, such as ubuntu's
// esm ones. They do not go through http_proxy.
package inrelease

import (
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"sync"
	"sync/atomic"
)

// Proxy is the running proxy. Close it when the run is over.
type Proxy struct {
	// URL is what http_proxy takes.
	URL string

	upstream http.RoundTripper
	srv      *http.Server
	mu       sync.Mutex
	entries  map[string]*entry
	fetched  atomic.Int64
	served   atomic.Int64
}

// entry is one InRelease, fetched once: whoever asks while it is being
// fetched waits for it.
type entry struct {
	once   sync.Once
	ok     bool
	header http.Header
	body   []byte
}

// Start listens on a port of its own on the loopback and serves, fetching
// through upstream, or when that is nil as http.DefaultTransport does.
func Start(upstream http.RoundTripper) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if upstream == nil {
		// The process's own proxy settings are the user's, and upstream
		// of this one: the chisels alone are pointed here.
		upstream = http.DefaultTransport.(*http.Transport).Clone()
	}
	p := &Proxy{URL: "http://" + ln.Addr().String(), upstream: upstream, entries: map[string]*entry{}}
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// Close stops the proxy.
func (p *Proxy) Close() error { return p.srv.Close() }

// Counts is how many InRelease files were fetched from the archive and how
// many requests for one were answered from memory.
func (p *Proxy) Counts() (fetched, served int) {
	return int(p.fetched.Load()), int(p.served.Load())
}

// cached reports whether a request is one to remember the answer to.
func cached(r *http.Request) bool {
	return r.Method == http.MethodGet && path.Base(r.URL.Path) == "InRelease" && r.Header.Get("Range") == ""
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		// https does not use http_proxy, so a CONNECT is not chisel's.
		http.Error(w, "CONNECT not supported", http.StatusMethodNotAllowed)
		return
	}
	if !cached(r) {
		p.pass(w, r)
		return
	}
	key := r.URL.String()
	p.mu.Lock()
	e := p.entries[key]
	if e == nil {
		e = &entry{}
		p.entries[key] = e
	}
	p.mu.Unlock()
	first := false
	e.once.Do(func() {
		first = true
		resp, err := p.roundTrip(r)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			return
		}
		e.ok, e.header, e.body = true, resp.Header.Clone(), body
	})
	if !e.ok {
		// Nothing to remember: the next one to ask tries again, and this
		// one gets the archive's own answer, whatever it is.
		p.mu.Lock()
		if p.entries[key] == e {
			delete(p.entries, key)
		}
		p.mu.Unlock()
		p.pass(w, r)
		return
	}
	if first {
		p.fetched.Add(1)
	} else {
		p.served.Add(1)
	}
	copyHeader(w.Header(), e.header)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.body)
}

// pass sends the request on and streams the answer back.
func (p *Proxy) pass(w http.ResponseWriter, r *http.Request) {
	resp, err := p.roundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) roundTrip(r *http.Request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	for _, h := range hopByHop {
		req.Header.Del(h)
	}
	return p.upstream.RoundTrip(req)
}

// hopByHop are the headers of one connection, not of the request.
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if slices.Contains(hopByHop, k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
