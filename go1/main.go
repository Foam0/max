package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"max-relay/dcnet"
)

// WS endpoint for side A (port 9000). Talks only to its browser tab.
// Browser inject bridges WS <-> WebRTC datachannel.

var up = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "HTTP listen address")
	wsPath := flag.String("ws-path", "/ws", "WebSocket path")
	proxyListen := flag.String("proxy-listen", "127.0.0.1:8080", "HTTP proxy listen address")
	stats := flag.Bool("stats", true, "log transport stats")
	statsInterval := flag.Duration("stats-interval", 5*time.Second, "interval for transport stats logs")
	flag.Parse()

	transport := dcnet.NewWSTransport(1024)
	mux := dcnet.NewMux(transport)
	proxySrv := &http.Server{
		Addr:    *proxyListen,
		Handler: newHTTPProxy(mux),
	}
	go func() {
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("proxy ListenAndServe: %v", err)
		}
	}()
	log.Printf("proxy: http on %s", *proxyListen)

	http.HandleFunc(*wsPath, func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			log.Println("upgrade:", err)
			return
		}
		transport.SetConn(c)
		log.Println("A connected")
	})

	srv := &http.Server{Addr: *listen}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	log.Printf("go1: ws on %s%s", *listen, *wsPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *stats {
		go logTransportStats(ctx, "go1", transport, *statsInterval)
	}
	<-ctx.Done()

	log.Println("shutting down")
	_ = srv.Shutdown(context.Background())
	_ = proxySrv.Shutdown(context.Background())
	_ = mux.Close()
}

func logTransportStats(ctx context.Context, label string, transport *dcnet.WSTransport, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastTx, lastRx uint64
	for {
		tx, rx := transport.Stats()
		dtx := tx - lastTx
		drx := rx - lastRx
		log.Printf("[%s] transport bytes tx=%d (+%d) rx=%d (+%d)", label, tx, dtx, rx, drx)
		lastTx, lastRx = tx, rx
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func newHTTPProxy(mux *dcnet.Mux) http.Handler {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return mux.DialContext(ctx, addr)
		},
		ForceAttemptHTTP2: false,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, mux)
			return
		}
		handleHTTP(w, r, transport)
	})
}

func handleConnect(w http.ResponseWriter, r *http.Request, mux *dcnet.Mux) {
	addr := r.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	upstream, err := mux.DialContext(r.Context(), addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		_ = upstream.Close()
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		_ = upstream.Close()
		return
	}
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	pipe := func(dst io.Writer, src io.Reader, closeFn func()) {
		_, _ = io.Copy(dst, src)
		closeFn()
	}
	closeBoth := func() {
		_ = clientConn.Close()
		_ = upstream.Close()
	}
	go pipe(upstream, clientConn, closeBoth)
	go pipe(clientConn, upstream, closeBoth)
}

func handleHTTP(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	req := r.Clone(r.Context())
	req.RequestURI = ""
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	removeHopHeaders(req.Header)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyHeader(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

func removeHopHeaders(h http.Header) {
	hop := []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}
	for _, k := range hop {
		h.Del(k)
	}
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
}
