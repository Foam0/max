package dcnet

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNoConn = errors.New("ws: no active connection")

type WSTransport struct {
	mu        sync.RWMutex
	conn      *websocket.Conn
	writeMu   sync.Mutex
	recvCh    chan []byte
	done      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
	txBytes   atomic.Uint64
	rxBytes   atomic.Uint64
}

func NewWSTransport(queue int) *WSTransport {
	if queue <= 0 {
		queue = 1024
	}
	return &WSTransport{
		recvCh: make(chan []byte, queue),
		done:   make(chan struct{}),
	}
}

func (t *WSTransport) SetConn(c *websocket.Conn) {
	t.mu.Lock()
	if t.conn != nil {
		_ = t.conn.Close()
	}
	t.conn = c
	t.mu.Unlock()

	go t.readLoop(c)
}

func (t *WSTransport) readLoop(c *websocket.Conn) {
	for {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		t.rxBytes.Add(uint64(len(msg)))
		buf := append([]byte(nil), msg...)
		select {
		case t.recvCh <- buf:
		case <-t.done:
			return
		}
	}

	t.mu.Lock()
	if t.conn == c {
		t.conn = nil
	}
	t.mu.Unlock()
}

func (t *WSTransport) Write(p []byte) error {
	if t.closed.Load() {
		return net.ErrClosed
	}
	t.mu.RLock()
	c := t.conn
	t.mu.RUnlock()
	if c == nil {
		return ErrNoConn
	}

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	if t.closed.Load() {
		return net.ErrClosed
	}
	if err := c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return err
	}
	t.txBytes.Add(uint64(len(p)))
	return nil
}

func (t *WSTransport) Read(deadline time.Time) ([]byte, error) {
	if t.closed.Load() {
		return nil, net.ErrClosed
	}
	if deadline.IsZero() {
		select {
		case p := <-t.recvCh:
			return p, nil
		case <-t.done:
			return nil, net.ErrClosed
		}
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case p := <-t.recvCh:
		return p, nil
	case <-timer.C:
		return nil, os.ErrDeadlineExceeded
	case <-t.done:
		return nil, net.ErrClosed
	}
}

func (t *WSTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		close(t.done)
		t.mu.Lock()
		if t.conn != nil {
			_ = t.conn.Close()
			t.conn = nil
		}
		t.mu.Unlock()
	})
	return nil
}

func (t *WSTransport) Stats() (txBytes uint64, rxBytes uint64) {
	return t.txBytes.Load(), t.rxBytes.Load()
}

type dummyAddr struct{}

func (dummyAddr) Network() string {
	return "dc"
}

func (dummyAddr) String() string {
	return "dc"
}
