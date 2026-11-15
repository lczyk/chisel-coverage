// Package suitecache spares a run of many chisel cuts the fetches every one
// of them repeats. chisel takes packages and their indexes from its cache,
// by digest, but asks the archive for each suite's InRelease anew on every
// cut, which is most of what a small cut waits for. The package is an HTTP
// forward proxy for the chisels of one run, reached through http_proxy,
// which is the only lever: it answers an InRelease it has fetched before
// from memory and passes everything else through as it is.
//
// One run then sees one state of the archive. An index chisel has not
// cached is still fetched from the archive as it is then, and chisel checks
// it against the InRelease it was given, so an archive published in the
// middle of a run fails a cut, loudly, where without the proxy only the cuts
// started after it would see the new state.
//
// Archives reached over https, as ubuntu's esm ones, do not go through
// http_proxy and are not seen here.
package suitecache

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
