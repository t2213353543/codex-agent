package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const gatewayClientPoolLimit = 128
const gatewayClientPoolIdle = 10 * time.Minute

type gatewayClientMode uint8

const (
	gatewayClientHTTP gatewayClientMode = iota
	gatewayClientCompact
	gatewayClientWebSocket
)

var errGatewayProxyUnavailable = errors.New("outbound proxy configuration changed or is unavailable")

type gatewayClientKey struct {
	proxy         string
	mode          gatewayClientMode
	headerTimeout time.Duration
}

type gatewayClientEntry struct {
	client    *http.Client
	base      *http.Client
	transport *gatewayClientTransport
	lastUsed  time.Time
	use       uint64
}

// Include the secret fields in a digest, never in a printable pool key. A
// version change also retires clients after disable/re-enable or an ABA edit.
func gatewayProxyFingerprint(proxy ProxyEntry) [32]byte {
	data, _ := json.Marshal(struct {
		Type, Host, Username, Password string
		Port                           int
		Version                        int64
		Enabled                        bool
	}{proxy.Type, proxy.Host, proxy.Username, proxy.Password, proxy.Port, proxy.Version, proxy.Enabled})
	return sha256.Sum256(data)
}

func (p *Proxy) compactClientFor(proxy *ProxyEntry) *http.Client {
	return p.gatewayClientFor(proxy, gatewayClientCompact)
}

func (p *Proxy) websocketClientFor(proxy *ProxyEntry) *http.Client {
	return p.gatewayClientFor(proxy, gatewayClientWebSocket)
}

func (p *Proxy) gatewayClientFor(proxy *ProxyEntry, mode gatewayClientMode) *http.Client {
	return p.gatewayClientForConfigured(proxy, mode, nil)
}

// Timeout variants share the same bounded pool as proxy/protocol variants.
// A nil timeout retains the mode's existing default; zero remains a valid
// explicit override. Existing transports are never mutated when settings change.
func (p *Proxy) gatewayClientForConfigured(proxy *ProxyEntry, mode gatewayClientMode, headerTimeout *time.Duration) *http.Client {
	if p.NativeHTTP != nil && mode != gatewayClientWebSocket {
		return p.gatewayNativeClientForConfigured(proxy, mode, headerTimeout)
	}
	base := p.Client
	if base == nil {
		base = http.DefaultClient
	}
	roundTripper := base.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	original, ok := roundTripper.(*http.Transport)
	if !ok && proxy == nil {
		// Injected RoundTrippers own header deadlines. Preserve their exact
		// direct HTTP client and never bypass them to create a pooled variant.
		if mode == gatewayClientHTTP {
			return base
		}
		client := newGatewayProxyClient(base, nil, mode)
		if mode == gatewayClientCompact && headerTimeout != nil {
			client.Timeout = *headerTimeout
		}
		return client
	}
	if !ok {
		original = http.DefaultTransport.(*http.Transport)
	}
	timeout := original.ResponseHeaderTimeout
	if mode == gatewayClientCompact {
		timeout = upstreamCompactTimeout
	}
	if headerTimeout != nil {
		timeout = *headerTimeout
	}
	// Keep the caller's default direct client and its established connections.
	if proxy == nil && mode == gatewayClientHTTP && timeout == original.ResponseHeaderTimeout {
		return base
	}

	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	var snapshot ProxyEntry
	var fingerprint [32]byte
	var store *ProxyStore
	key := gatewayClientKey{mode: mode, headerTimeout: timeout}
	if proxy != nil {
		snapshot = *proxy
		if proxy.ID != "" && p.Proxies != nil {
			// Resolve under the pool lock so an older selected snapshot cannot
			// replace a client inserted by a concurrent, newer selection.
			current, ok := p.Proxies.Get(proxy.ID)
			if !ok {
				p.retireGatewayRouteLocked(proxy.ID)
				return &http.Client{Transport: gatewayClientErrorTransport{}}
			}
			snapshot, store = *current, p.Proxies
		}
		fingerprint = gatewayProxyFingerprint(snapshot)
		key.proxy = snapshot.ID
		if key.proxy == "" {
			key.proxy = string(fingerprint[:])
		}
		proxy = &snapshot
	}
	if p.clientPool == nil {
		p.clientPool = make(map[gatewayClientKey]*gatewayClientEntry)
	}
	now := time.Now()
	p.clientUse++
	for cachedKey, entry := range p.clientPool {
		if now.Sub(entry.lastUsed) >= gatewayClientPoolIdle || entry.base != base ||
			(cachedKey.proxy == key.proxy && (entry.transport.fingerprint != fingerprint || entry.transport.store != store)) {
			entry.transport.retire()
			delete(p.clientPool, cachedKey)
		}
	}
	if entry := p.clientPool[key]; entry != nil {
		entry.lastUsed = now
		entry.use = p.clientUse
		return entry.client
	}
	if len(p.clientPool) >= gatewayClientPoolLimit {
		var oldest gatewayClientKey
		var oldestUse uint64
		for cachedKey, entry := range p.clientPool {
			if oldestUse == 0 || entry.use < oldestUse {
				oldest, oldestUse = cachedKey, entry.use
			}
		}
		p.clientPool[oldest].transport.retire()
		delete(p.clientPool, oldest)
	}
	client := newGatewayProxyClient(base, proxy, mode)
	transport := client.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = timeout
	if mode == gatewayClientCompact {
		client.Timeout = timeout
	}
	// Reclaim sockets even if the pool receives no further traffic. Entry
	// metadata is bounded and is reclaimed lazily or at service shutdown.
	if transport.IdleConnTimeout <= 0 || transport.IdleConnTimeout > 90*time.Second {
		transport.IdleConnTimeout = 90 * time.Second
	}
	pooled := &gatewayClientTransport{transport: transport, store: store, proxyID: snapshot.ID, fingerprint: fingerprint}
	client.Transport = pooled
	p.clientPool[key] = &gatewayClientEntry{client: client, base: base, transport: pooled, lastUsed: now, use: p.clientUse}
	return client
}

func (p *Proxy) retireGatewayRouteLocked(id string) {
	for key, entry := range p.clientPool {
		if key.proxy == id {
			entry.transport.retire()
			delete(p.clientPool, key)
		}
	}
}

// CloseIdleConnections never interrupts an active response or an upgraded WS.
func (p *Proxy) closeGatewayClients() {
	if p.NativeHTTP != nil {
		_ = p.NativeHTTP.Close()
	}
	// The default direct client is deliberately outside clientPool. It owns
	// sockets too, including connections created before native HTTP was enabled.
	if p.Client != nil {
		p.Client.CloseIdleConnections()
	}
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	for key, entry := range p.clientPool {
		entry.transport.retire()
		delete(p.clientPool, key)
	}
}

// newGatewayProxyClient is deliberately unpooled for admin diagnostics, which
// install CONNECT observers and must not mutate or close gateway transports.
func newGatewayProxyClient(base *http.Client, proxy *ProxyEntry, mode gatewayClientMode) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	roundTripper := base.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok && proxy != nil {
		transport = http.DefaultTransport.(*http.Transport)
		ok = true
	}
	if ok {
		transport = transport.Clone()
		if proxy != nil {
			switch proxy.Type {
			case "socks5":
				dialer := socks5Dialer{proxyAddr: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port)), username: proxy.Username, password: proxy.Password}
				transport.DialContext, transport.Proxy = dialer.DialContext, nil
				// A custom direct TLS dialer takes precedence over DialContext
				// when Proxy is nil and would silently bypass the SOCKS route.
				transport.DialTLS, transport.DialTLSContext = nil, nil
			default:
				target := &url.URL{Scheme: "http", Host: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))}
				if proxy.Username != "" {
					target.User = url.UserPassword(proxy.Username, proxy.Password)
				}
				transport.Proxy = http.ProxyURL(target)
			}
		}
		if mode == gatewayClientCompact {
			transport.ResponseHeaderTimeout = upstreamCompactTimeout
		}
		client.Transport = transport
	}
	if mode == gatewayClientCompact {
		client.Timeout = upstreamCompactTimeout
	}
	if mode == gatewayClientWebSocket {
		return websocketHTTPClient(&client)
	}
	return &client
}

type gatewayClientErrorTransport struct{}

func (gatewayClientErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, errGatewayProxyUnavailable
}

type gatewayClientTransport struct {
	transport   *http.Transport
	store       *ProxyStore
	proxyID     string
	fingerprint [32]byte
	retired     atomic.Bool
}

func (t *gatewayClientTransport) retire() {
	t.retired.Store(true)
	t.transport.CloseIdleConnections()
}

func (t *gatewayClientTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

func (t *gatewayClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Recheck when the request actually starts: a caller may hold a client
	// while the proxy is disabled, deleted, or has its credentials rotated.
	if t.store != nil {
		current, ok := t.store.Get(t.proxyID)
		if !ok || gatewayProxyFingerprint(*current) != t.fingerprint {
			t.retire()
			return (gatewayClientErrorTransport{}).RoundTrip(req)
		}
	}
	res, err := t.transport.RoundTrip(req)
	if err != nil {
		t.closeRetiredIdle()
		return res, err
	}
	body := &gatewayClientBody{ReadCloser: res.Body, transport: t}
	if writer, ok := res.Body.(io.Writer); ok {
		// coder/websocket needs the HTTP 101 body's bidirectional interface.
		res.Body = &gatewayClientDuplexBody{gatewayClientBody: body, writer: writer}
	} else {
		res.Body = body
	}
	return res, nil
}

func (t *gatewayClientTransport) closeRetiredIdle() {
	if t.retired.Load() {
		t.transport.CloseIdleConnections()
	}
}

type gatewayClientBody struct {
	io.ReadCloser
	transport *gatewayClientTransport
	once      sync.Once
	err       error
}

func (b *gatewayClientBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.transport.closeRetiredIdle()
	}
	return n, err
}

func (b *gatewayClientBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.transport.closeRetiredIdle()
	})
	return b.err
}

type gatewayClientDuplexBody struct {
	*gatewayClientBody
	writer io.Writer
}

func (b *gatewayClientDuplexBody) Write(p []byte) (int, error) { return b.writer.Write(p) }
