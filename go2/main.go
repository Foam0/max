package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"max-relay/dcnet"
)

// WS endpoint for side B (port 9001). Talks only to its browser tab.
// Browser inject bridges WS <-> WebRTC datachannel.

var up = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9001", "HTTP listen address")
	wsPath := flag.String("ws-path", "/ws", "WebSocket path")
	netCheck := flag.Bool("net-check", true, "periodically check outbound connectivity")
	netCheckAddr := flag.String("net-check-addr", "1.1.1.1:443", "TCP address for connectivity check")
	netCheckInterval := flag.Duration("net-check-interval", 10*time.Second, "interval for network check")
	netCheckTimeout := flag.Duration("net-check-timeout", 2*time.Second, "timeout for network check")
	proxyDialTimeout := flag.Duration("proxy-dial-timeout", 10*time.Second, "dial timeout for proxy egress")
	stats := flag.Bool("stats", true, "log transport stats")
	statsInterval := flag.Duration("stats-interval", 5*time.Second, "interval for transport stats logs")
	flag.Parse()

	transport := dcnet.NewWSTransport(1024)
	mux := dcnet.NewMux(transport)
	mux.SetServerDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: *proxyDialTimeout}
		return dialer.DialContext(ctx, "tcp", addr)
	})
	log.Printf("proxy: egress enabled (timeout %s)", proxyDialTimeout.String())

	http.HandleFunc(*wsPath, func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			log.Println("upgrade:", err)
			return
		}
		transport.SetConn(c)
		log.Println("B connected")
	})

	srv := &http.Server{Addr: *listen}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	log.Printf("go2: ws on %s%s", *listen, *wsPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *netCheck {
		go netCheckLoop(ctx, *netCheckAddr, *netCheckInterval, *netCheckTimeout)
	}
	if *stats {
		go logTransportStats(ctx, "go2", transport, *statsInterval)
	}
	<-ctx.Done()

	log.Println("shutting down")
	_ = srv.Shutdown(context.Background())
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

func netCheckLoop(ctx context.Context, addr string, interval, timeout time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var last bool
	first := true
	for {
		ok := checkNetwork(addr, timeout)
		if first || ok != last {
			if ok {
				log.Printf("net-check: online (%s)", addr)
			} else {
				log.Printf("net-check: offline (%s)", addr)
			}
			last = ok
			first = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func checkNetwork(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
