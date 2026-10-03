package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This bridge only changes the proxy handshake. HTTPS payloads remain an
// opaque TCP tunnel, including the Rust client's complete TLS exchange.
type gatewayNativeSOCKSBridge struct {
	mu        sync.Mutex
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
	forward   *httputil.ReverseProxy
	dialer    socks5Dialer
	origin    *url.URL
	authority string
	proxyURL  string
	auth      string
	timeout   time.Duration
	context   context.Context
	cancel    context.CancelFunc
	closed    bool
	clients   map[*gatewayNativeSOCKSConn]struct{}
	done      chan struct{}
	handlers  sync.WaitGroup
	closeOnce sync.Once
}

func newGatewayNativeSOCKSBridge(proxy ProxyEntry, origin string, headerTimeout time.Duration) (*gatewayNativeSOCKSBridge, error) {
	target, err := url.Parse(origin)
	if err != nil || target.User != nil || target.RawQuery != "" || target.ForceQuery || strings.Contains(origin, "#") || target.Path != "" && target.Path != "/" ||
		(target.Scheme != "http" && target.Scheme != "https") || proxy.Type != "socks5" || proxy.Host == "" || proxy.Port < 1 || proxy.Port > 65535 ||
		len(proxy.Username) > 255 || len(proxy.Password) > 255 || proxy.Username == "" && proxy.Password != "" || headerTimeout < 0 {
		return nil, errGatewayNativeProxy
	}
	authority, err := gatewayNativeSOCKSAuthority(target.Host, target.Scheme)
	if err != nil {
		return nil, errGatewayNativeProxy
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, errGatewayNativeUnavailable
	}
	username, password := hex.EncodeToString(random[:16]), hex.EncodeToString(random[16:])
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errGatewayNativeUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &gatewayNativeSOCKSBridge{listener: listener, origin: target, authority: authority, timeout: headerTimeout,
		dialer: socks5Dialer{proxyAddr: net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port)), username: proxy.Username, password: proxy.Password},
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password)), context: ctx, cancel: cancel,
		clients: make(map[*gatewayNativeSOCKSConn]struct{}), done: make(chan struct{})}
	endpoint := &url.URL{Scheme: "http", Host: listener.Addr().String(), User: url.UserPassword(username, password)}
	b.proxyURL = endpoint.String()
	b.transport = &http.Transport{Proxy: nil, DialContext: b.dialContext, DisableCompression: true,
		DisableKeepAlives: true, ForceAttemptHTTP2: false,
		ResponseHeaderTimeout: headerTimeout}
	// Plain HTTP necessarily passes through net/http serialization. Keep raw
	// entity bytes and repeated headers, but do not claim framing identity or
	// reuse stale sockets where net/http could retry an application request.
	b.forward = &httputil.ReverseProxy{Transport: b.transport, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.Header.Del("Proxy-Authorization")
			// ReverseProxy cleans malformed escapes and semicolon query pairs
			// before Rewrite. This is a fixed-origin transport bridge, so retain
			// the caller's URI just as the CLI transport sends it.
			request.Out.URL.RawQuery = request.In.URL.RawQuery
			// net/http detaches its dial context from request cancellation for
			// connection reuse. This bridge disables reuse, so retain the request
			// lifetime explicitly and stop a canceled SOCKS negotiation promptly.
			request.Out = request.Out.WithContext(context.WithValue(request.Out.Context(), gatewayNativeSOCKSDialContextKey{}, request.In.Context()))
			if len(request.Out.Trailer) > 0 {
				for key := range request.Out.Trailer {
					if !gatewayNativeSOCKSTrailerAllowed(key, request.In.Header.Values("Connection")) {
						delete(request.Out.Trailer, key)
					}
				}
				if request.Out.Body != nil {
					request.Out.Body = &gatewayNativeSOCKSTrailerBody{ReadCloser: request.Out.Body, incoming: request.In, outgoing: request.Out.Trailer}
				}
			}
		}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "SOCKS route unavailable", http.StatusBadGateway)
		}}
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: gatewayNativeHeadLimit, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return b.context }}
	go func() { _ = b.server.Serve(&gatewayNativeSOCKSListener{bridge: b}); close(b.done) }()
	return b, nil
}

func gatewayNativeSOCKSAuthority(authority, scheme string) (string, error) {
	if (scheme != "http" && scheme != "https") || strings.HasSuffix(authority, ":") || strings.ContainsAny(authority, "\\/?#@\r\n\t ") ||
		!strings.HasPrefix(authority, "[") && strings.Count(authority, ":") > 1 {
		return "", errGatewayNativeProxy
	}
	parsed, err := url.Parse(scheme + "://" + authority)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery {
		return "", errGatewayNativeProxy
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	number, err := strconv.Atoi(port)
	host := parsed.Hostname()
	if err != nil || socks5ValidateTarget(host, number) != nil {
		return "", errGatewayNativeProxy
	}
	if ip := net.ParseIP(host); ip != nil {
		if strings.HasPrefix(authority, "[") && !strings.Contains(host, ":") {
			return "", errGatewayNativeProxy
		}
		host = ip.String()
	} else if strings.ContainsAny(authority, "[]") {
		return "", errGatewayNativeProxy
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(number)), nil
}

func (b *gatewayNativeSOCKSBridge) ProxyURL() string { return b.proxyURL }

type gatewayNativeSOCKSDialContextKey struct{}

// Request.Clone copies the trailer map before its values have arrived. Copy
// declared end-to-end trailers at EOF, on the same goroutine writing the body.
type gatewayNativeSOCKSTrailerBody struct {
	io.ReadCloser
	incoming *http.Request
	outgoing http.Header
}

func (b *gatewayNativeSOCKSTrailerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		for key := range b.outgoing {
			b.outgoing[key] = append([]string(nil), b.incoming.Trailer.Values(key)...)
		}
	}
	return n, err
}

func gatewayNativeSOCKSTrailerAllowed(key string, connection []string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authorization", "proxy-authenticate", "authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return false
	}
	for _, value := range connection {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), key) {
				return false
			}
		}
	}
	return true
}

func (b *gatewayNativeSOCKSBridge) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	authority, err := gatewayNativeSOCKSAuthority(address, b.origin.Scheme)
	if err != nil || authority != b.authority {
		return nil, errGatewayNativeProxy
	}
	if requestContext, ok := ctx.Value(gatewayNativeSOCKSDialContextKey{}).(context.Context); ok {
		if err := requestContext.Err(); err != nil {
			return nil, err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		stop := context.AfterFunc(requestContext, cancel)
		defer func() { stop(); cancel() }()
	}
	if b.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}
	return b.dialer.DialContext(ctx, network, b.authority)
}

func (b *gatewayNativeSOCKSBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		http.Error(w, "SOCKS bridge closed", http.StatusServiceUnavailable)
		return
	}
	b.handlers.Add(1)
	b.mu.Unlock()
	defer b.handlers.Done()
	credentials := r.Header.Values("Proxy-Authorization")
	if len(credentials) != 1 || subtle.ConstantTimeCompare([]byte(credentials[0]), []byte(b.auth)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="native-route"`)
		http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		authority, err := gatewayNativeSOCKSAuthority(r.RequestURI, b.origin.Scheme)
		host, hostErr := gatewayNativeSOCKSAuthority(r.Host, b.origin.Scheme)
		if err != nil || hostErr != nil || authority != b.authority || host != b.authority ||
			r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "Proxy target not allowed", http.StatusForbidden)
			return
		}
		b.tunnel(w, r)
		return
	}
	authority, err := gatewayNativeSOCKSAuthority(r.URL.Host, r.URL.Scheme)
	host, hostErr := gatewayNativeSOCKSAuthority(r.Host, "http")
	if b.origin.Scheme != "http" || r.URL.Scheme != "http" || r.URL.User != nil || r.URL.Fragment != "" ||
		err != nil || hostErr != nil || authority != b.authority || host != b.authority || r.Header.Get("Upgrade") != "" || r.Method == http.MethodTrace {
		http.Error(w, "Proxy target not allowed", http.StatusForbidden)
		return
	}
	b.forward.ServeHTTP(w, r)
}

func (b *gatewayNativeSOCKSBridge) tunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := b.dialContext(r.Context(), "tcp", b.authority)
	if err != nil {
		http.Error(w, "SOCKS route unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	// Hijacked sockets no longer belong to http.Server.Close. Shutdown must
	// also interrupt an upstream read after the client has half-closed.
	stopCancellation := context.AfterFunc(b.context, func() {
		_ = upstream.Close()
		_ = client.Close()
	})
	defer stopCancellation()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	finished := make(chan struct{}, 1)
	go func() {
		_, err := io.Copy(upstream, buffered)
		if err == nil {
			_ = gatewayNativeSOCKSCloseWrite(upstream)
		} else {
			_ = upstream.Close()
			_ = client.Close()
		}
		finished <- struct{}{}
	}()
	if _, err := io.Copy(client, upstream); err == nil {
		_ = gatewayNativeSOCKSCloseWrite(client)
	} else {
		_ = upstream.Close()
		_ = client.Close()
	}
	<-finished
}

func gatewayNativeSOCKSCloseWrite(conn net.Conn) error {
	if conn, ok := conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return conn.Close()
}

func (b *gatewayNativeSOCKSBridge) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		clients := make([]*gatewayNativeSOCKSConn, 0, len(b.clients))
		for client := range b.clients {
			clients = append(clients, client)
		}
		b.mu.Unlock()
		b.cancel()
		_ = b.listener.Close()
		_ = b.server.Close()
		for _, client := range clients {
			_ = client.Close()
		}
		b.transport.CloseIdleConnections()
		<-b.done
		b.handlers.Wait()
	})
	return nil
}

type gatewayNativeSOCKSListener struct{ bridge *gatewayNativeSOCKSBridge }

func (l *gatewayNativeSOCKSListener) Accept() (net.Conn, error) {
	b := l.bridge
	// The account admission layer already owns concurrency. The bridge must
	// accept every admitted HTTP request or WebSocket tunnel on this route.
	conn, err := b.listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &gatewayNativeSOCKSConn{Conn: conn, bridge: b}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	b.clients[wrapped] = struct{}{}
	b.mu.Unlock()
	return wrapped, nil
}

func (l *gatewayNativeSOCKSListener) Close() error   { return l.bridge.listener.Close() }
func (l *gatewayNativeSOCKSListener) Addr() net.Addr { return l.bridge.listener.Addr() }

type gatewayNativeSOCKSConn struct {
	net.Conn
	bridge *gatewayNativeSOCKSBridge
	once   sync.Once
	err    error
}

func (c *gatewayNativeSOCKSConn) CloseWrite() error {
	return gatewayNativeSOCKSCloseWrite(c.Conn)
}

func (c *gatewayNativeSOCKSConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.bridge.mu.Lock()
		delete(c.bridge.clients, c)
		c.bridge.mu.Unlock()
	})
	return c.err
}
