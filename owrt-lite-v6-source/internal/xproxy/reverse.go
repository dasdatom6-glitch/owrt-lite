package xproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/core"
)

type Reverse struct {
	transport *http.Transport
	proxy     *httputil.ReverseProxy
	nm        LogAdder

	connMu      sync.Mutex
	activeConns map[net.Conn]struct{}
}

type LogAdder interface {
	AddLog(msg string)
}

func splitHostPortDefault(addr string, defaultPort string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		portStr = defaultPort
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func appendSocksAddr(buf []byte, host string, port int) []byte {
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		buf = append(buf, 0x01)
		buf = append(buf, ip4...)
	} else if ip6 := ip.To16(); ip6 != nil {
		buf = append(buf, 0x04)
		buf = append(buf, ip6...)
	} else {
		if len(host) > 255 {
			host = host[:255]
		}
		buf = append(buf, 0x03, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = append(buf, byte(port>>8), byte(port&0xff))
	return buf
}

func trojanPassword(node *url.URL) string {
	if node.User == nil {
		return ""
	}
	if p, ok := node.User.Password(); ok && p != "" {
		return p
	}
	return node.User.Username()
}

func boolQuery(v url.Values, keys ...string) bool {
	for _, k := range keys {
		s := strings.ToLower(v.Get(k))
		if s == "1" || s == "true" || s == "yes" {
			return true
		}
	}
	return false
}

func decodeSSUserInfo(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.URLEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	}
	if !strings.Contains(raw, ":") {
		return "", "", false
	}
	parts := strings.SplitN(raw, ":", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func ssCredentials(node *url.URL) (string, string, error) {
	if node.User == nil {
		return "", "", fmt.Errorf("ss node missing cipher/password")
	}
	method := node.User.Username()
	password, hasPassword := node.User.Password()
	if hasPassword && password != "" {
		return method, password, nil
	}
	if m, p, ok := decodeSSUserInfo(method); ok {
		return m, p, nil
	}
	return method, password, nil
}

// Custom DialContext for chaining
func (r *Reverse) dialChain(ctx context.Context, network, addr string, nodes []*url.URL) (net.Conn, error) {
	if len(nodes) == 0 {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	// Dial the first node
	first := nodes[0]
	var conn net.Conn
	var err error

	// Connect to first hop
	d := &net.Dialer{Timeout: 15 * time.Second}
	conn, err = d.DialContext(ctx, "tcp", first.Host)
	if err != nil {
		return nil, fmt.Errorf("dial first hop %s: %w", first.Host, err)
	}

	// Handshake with first hop (and subsequent hops)
	for i, node := range nodes {
		nextAddr := addr
		if i < len(nodes)-1 {
			nextAddr = nodes[i+1].Host
		}
		fmt.Printf("[CHAIN] Hop %d: %s -> %s\n", i, node.String(), nextAddr)

		switch strings.ToLower(node.Scheme) {
		case "ss":
			// Shadowsocks. Supports both ss://method:pass@host and SIP002 ss://base64(method:pass)@host.
			method, password, credErr := ssCredentials(node)
			if credErr != nil {
				conn.Close()
				return nil, credErr
			}
			ciph, err := core.PickCipher(method, nil, password)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("ss cipher err: %w", err)
			}
			// Wrap connection
			conn = ciph.StreamConn(conn)

			// Dial target inside the tunnel
			// SS protocol: [Target Address] ...
			// However, we are in a chain loop.
			// If we are chaining:
			//   We have a SS connection to Node A.
			//   We want to connect to Node B (nextAddr) through Node A.
			//   SS client simply writes the target address.
			//   The `conn` we have now is an encrypted stream.
			//   We need to write the SS target address format.
			//   core.Dialer usually does this.

			// We can't use core.Dialer because we already have a conn.
			// We need to write the address manually.
			// SS address format: [Type][Addr][Port]

			// Parse nextAddr
			host, portStr, err := net.SplitHostPort(nextAddr)
			if err != nil {
				// Fallback if port is missing
				host = nextAddr
				portStr = "80" // Default port
				if node.Scheme == "ss" {
					portStr = "8388"
				} else if node.Scheme == "socks5" {
					portStr = "1080"
				}
			}
			tgtHost := host
			tgtPort, _ := net.LookupPort("tcp", portStr)

			// Form address buffer
			// 1 byte type (1=IPv4, 3=Domain, 4=IPv6)
			// ...
			// Fortunately, go-shadowsocks2/socks has standard address parsing/writing but it is internal in some packages?
			// Let's implement simple address writing.

			buf := []byte{}
			ip := net.ParseIP(tgtHost)
			if ip4 := ip.To4(); ip4 != nil {
				buf = append(buf, 1)
				buf = append(buf, ip4...)
			} else if ip6 := ip.To16(); ip6 != nil {
				buf = append(buf, 4)
				buf = append(buf, ip6...)
			} else {
				buf = append(buf, 3)
				buf = append(buf, byte(len(tgtHost)))
				buf = append(buf, tgtHost...)
			}
			buf = append(buf, byte(tgtPort>>8), byte(tgtPort&0xff))

			if _, err := conn.Write(buf); err != nil {
				conn.Close()
				return nil, err
			}
			// SS is established. The stream is now connected to nextAddr.
			// No handshake response reading for SS (it's stream).

		case "trojan":
			// Trojan protocol: TLS to server, then SHA224(password) + CRLF + CONNECT request.
			password := trojanPassword(node)
			if password == "" {
				conn.Close()
				return nil, fmt.Errorf("trojan node missing password")
			}
			q := node.Query()
			serverName := q.Get("sni")
			if serverName == "" {
				serverName = q.Get("peer")
			}
			if serverName == "" {
				serverName = node.Hostname()
			}
			tlsConn := tls.Client(conn, &tls.Config{
				ServerName:         serverName,
				InsecureSkipVerify: boolQuery(q, "allowInsecure", "insecure", "skip-cert-verify"),
			})
			if err := tlsConn.Handshake(); err != nil {
				conn.Close()
				return nil, fmt.Errorf("trojan tls handshake failed: %w", err)
			}
			conn = tlsConn

			host, port, err := splitHostPortDefault(nextAddr, "443")
			if err != nil {
				conn.Close()
				return nil, err
			}
			sum := sha256.Sum224([]byte(password))
			req := []byte(hex.EncodeToString(sum[:]))
			req = append(req, '\r', '\n')
			req = append(req, 0x01) // CONNECT
			req = appendSocksAddr(req, host, port)
			req = append(req, '\r', '\n')
			if _, err := conn.Write(req); err != nil {
				conn.Close()
				return nil, err
			}

		case "socks5":
			// SOCKS5 Handshake
			// We need to use the connection we have to dial the next hop.
			// x/net/proxy does not expose a way to use existing conn easily without registering a dialer.
			// Implementing minimal SOCKS5 client on existing conn:

			// 1. Auth (No auth or User/Pass)
			methods := []byte{0x00}
			if node.User != nil {
				methods = append(methods, 0x02)
			}
			if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
				conn.Close()
				return nil, err
			}
			buf := make([]byte, 2)
			if _, err := io.ReadFull(conn, buf); err != nil {
				conn.Close()
				return nil, err
			}
			if buf[0] != 0x05 {
				conn.Close()
				return nil, fmt.Errorf("socks5 ver err")
			}

			// User/Pass Auth
			if buf[1] == 0x02 {
				u := node.User.Username()
				p, _ := node.User.Password()
				payload := append([]byte{0x01, byte(len(u))}, u...)
				payload = append(payload, byte(len(p)))
				payload = append(payload, p...)
				if _, err := conn.Write(payload); err != nil {
					conn.Close()
					return nil, err
				}
				if _, err := io.ReadFull(conn, buf); err != nil {
					conn.Close()
					return nil, err
				}
				if buf[1] != 0x00 {
					conn.Close()
					return nil, fmt.Errorf("socks5 auth failed")
				}
			}

			// 2. Connect
			// Parse nextAddr (host:port)
			host, portStr, err := net.SplitHostPort(nextAddr)
			if err != nil {
				host = nextAddr
				portStr = "1080"
			}
			port, _ := net.LookupPort("tcp", portStr)

			req := []byte{0x05, 0x01, 0x00} // Ver, Cmd(Connect), Rsv
			ip := net.ParseIP(host)
			if ip4 := ip.To4(); ip4 != nil {
				req = append(req, 0x01)
				req = append(req, ip4...)
			} else if ip6 := ip.To16(); ip6 != nil {
				req = append(req, 0x04)
				req = append(req, ip6...)
			} else {
				req = append(req, 0x03, byte(len(host)))
				req = append(req, host...)
			}
			req = append(req, byte(port>>8), byte(port&0xff))

			if _, err := conn.Write(req); err != nil {
				conn.Close()
				return nil, err
			}
			// Read response
			// Ver, Rep, Rsv, Atyp, ...
			head := make([]byte, 4)
			if _, err := io.ReadFull(conn, head); err != nil {
				conn.Close()
				return nil, err
			}
			if head[1] != 0x00 {
				conn.Close()
				return nil, fmt.Errorf("socks5 connect failed: %d", head[1])
			}
			// Skip bind addr
			switch head[3] {
			case 0x01:
				io.ReadFull(conn, make([]byte, 4+2))
			case 0x04:
				io.ReadFull(conn, make([]byte, 16+2))
			case 0x03:
				l := make([]byte, 1)
				io.ReadFull(conn, l)
				io.ReadFull(conn, make([]byte, int(l[0])+2))
			}

		case "http", "https":
			// HTTP CONNECT. For HTTPS proxy servers, establish TLS to the proxy first.
			if strings.ToLower(node.Scheme) == "https" {
				serverName := node.Hostname()
				tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
				if err := tlsConn.Handshake(); err != nil {
					conn.Close()
					return nil, fmt.Errorf("https proxy tls handshake failed: %w", err)
				}
				conn = tlsConn
			}
			req := &http.Request{
				Method: "CONNECT",
				URL:    &url.URL{Host: nextAddr},
				Host:   nextAddr,
				Header: make(http.Header),
			}
			if node.User != nil {
				p, _ := node.User.Password()
				auth := base64.StdEncoding.EncodeToString([]byte(node.User.Username() + ":" + p))
				req.Header.Set("Proxy-Authorization", "Basic "+auth)
			}
			if err := req.Write(conn); err != nil {
				conn.Close()
				return nil, err
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if resp.StatusCode != 200 {
				conn.Close()
				return nil, fmt.Errorf("http connect failed: %s", resp.Status)
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("unsupported scheme: %s", node.Scheme)
		}
	}
	return conn, nil
}

func (r *Reverse) trackConn(c net.Conn) {
	if c == nil {
		return
	}
	r.connMu.Lock()
	r.activeConns[c] = struct{}{}
	r.connMu.Unlock()
}

func (r *Reverse) untrackConn(c net.Conn) {
	if c == nil {
		return
	}
	r.connMu.Lock()
	delete(r.activeConns, c)
	r.connMu.Unlock()
}

// CloseActiveConnections closes current CONNECT tunnels so that browsers/apps
// reconnect through the newly selected node immediately instead of reusing an
// old tunnel whose exit IP cannot change mid-connection.
func (r *Reverse) CloseActiveConnections() int {
	r.connMu.Lock()
	conns := make([]net.Conn, 0, len(r.activeConns))
	for c := range r.activeConns {
		conns = append(conns, c)
	}
	r.connMu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

func New(nm LogAdder) *Reverse {
	t := &http.Transport{
		Proxy:                 nil, // Disable system proxy to avoid loops
		MaxIdleConns:          256,
		IdleConnTimeout:       60 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &Reverse{
		transport:   t,
		proxy:       &httputil.ReverseProxy{Transport: t},
		nm:          nm,
		activeConns: make(map[net.Conn]struct{}),
	}
}

// ServeProxy handles both HTTP and CONNECT proxy requests
func (r *Reverse) ServeProxy(w http.ResponseWriter, req *http.Request, chain []*url.URL) {
	if req.Method == http.MethodConnect {
		r.handleConnect(w, req, chain)
	} else {
		r.ServeChain(w, req, chain)
	}
}

func (r *Reverse) handleConnect(w http.ResponseWriter, req *http.Request, chain []*url.URL) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Don't close conn here, it's hijacked

	// Connect to target via chain
	targetConn, err := r.dialChain(context.Background(), "tcp", req.Host, chain)
	if err != nil {
		msg := fmt.Sprintf("Chain Error: %s -> %v", req.Host, err)
		fmt.Println(msg)
		if r.nm != nil {
			r.nm.AddLog(msg)
		}
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		conn.Close()
		return
	}

	// Success
	r.trackConn(conn)
	r.trackConn(targetConn)
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Pipe
	go func() {
		defer r.untrackConn(conn)
		defer r.untrackConn(targetConn)
		defer conn.Close()
		defer targetConn.Close()
		io.Copy(conn, targetConn)
	}()
	go func() {
		defer r.untrackConn(conn)
		defer r.untrackConn(targetConn)
		defer conn.Close()
		defer targetConn.Close()
		io.Copy(targetConn, conn)
	}()
}

// ServeDirect handles direct connections (bypass proxy)
func (r *Reverse) ServeDirect(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		// Handle CONNECT directly
		dest := req.Host
		d := net.Dialer{Timeout: 10 * time.Second}
		targetConn, err := d.DialContext(req.Context(), "tcp", dest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer targetConn.Close()

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer clientConn.Close()

		r.trackConn(clientConn)
		r.trackConn(targetConn)
		defer r.untrackConn(clientConn)
		defer r.untrackConn(targetConn)

		// For CONNECT, we must write the established header manually
		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		// Pipe
		errCh := make(chan error, 2)
		go func() {
			_, err := io.Copy(targetConn, clientConn)
			errCh <- err
		}()
		go func() {
			_, err := io.Copy(clientConn, targetConn)
			errCh <- err
		}()
		<-errCh
	} else {
		// Handle HTTP directly
		rp := &httputil.ReverseProxy{
			Transport: &http.Transport{
				Proxy: nil, // Ensure we don't use system proxy
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			Director: func(rq *http.Request) {
				if rq.URL.Scheme == "" {
					rq.URL.Scheme = "http"
				}
				if rq.URL.Host == "" {
					rq.URL.Host = req.Host
				}
			},
		}
		rp.ServeHTTP(w, req)
	}
}

// ServeChain proxies the request through a chain of nodes
func (r *Reverse) ServeChain(w http.ResponseWriter, req *http.Request, chain []*url.URL) {
	// We need to use a custom Transport for this request that uses our chain dialer
	// Since we can't easily swap Transport per request in ReverseProxy without creating a new one,
	// and creating a new ReverseProxy per request is fine for low concurrency but maybe heavy.
	// However, for "lightweight" it's acceptable.

	rp := &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return r.dialChain(ctx, network, addr, chain)
			},
			MaxIdleConns:      10,
			IdleConnTimeout:   30 * time.Second,
			DisableKeepAlives: true,
		},
		Director: func(rq *http.Request) {
			// Forward Proxy logic: Keep the URL as is, or rewrite if needed.
			// For a forward proxy, the client sends absolute URI.
			// But here, we might be receiving a request to "/foo" and want to proxy it to "target/foo"?
			// No, the user likely uses this as a standard HTTP proxy or Reverse Proxy to a specific target.
			// If used as Reverse Proxy to "Target", we need to set the Target.
			// But ServeChain is generic.
			// If chain is A->B, and we want to visit C.
			// If request is already absolute (Forward Proxy mode), we leave it.
			// If request is relative, we assume the user wants to hit the Target defined in the request?
			// Wait, standard ReverseProxy usage implies we map incoming path to upstream path.
			// BUT, if we use a Chain, usually the LAST node is the exit, and we want to visit some Target.
			// Let's assume req.URL is the target.

			// Ensure Scheme/Host are set if missing (for relative requests)
			if rq.URL.Scheme == "" {
				rq.URL.Scheme = "http"
			}
			if rq.URL.Host == "" {
				rq.URL.Host = req.Host
			}
		},
	}
	rp.ServeHTTP(w, req)
}

// Legacy ServeHTTP for backward compatibility (single node)
func (r *Reverse) ServeHTTP(w http.ResponseWriter, req *http.Request, upstream *url.URL) {
	r.ServeChain(w, req, []*url.URL{upstream})
}
