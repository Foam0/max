package dcnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameOpen    = 1
	frameOpenOK  = 2
	frameOpenErr = 3
	frameData    = 4
	frameClose   = 5
)

const (
	frameHeaderLen  = 9
	maxFramePayload = 16 * 1024
)

type dialResult struct {
	conn net.Conn
	err  error
}

type Mux struct {
	transport *WSTransport

	mu      sync.Mutex
	streams map[uint32]*stream
	pending map[uint32]chan dialResult

	nextID atomic.Uint32
	closed atomic.Bool

	serverDial func(ctx context.Context, addr string) (net.Conn, error)
}

func NewMux(transport *WSTransport) *Mux {
	m := &Mux{
		transport: transport,
		streams:   make(map[uint32]*stream),
		pending:   make(map[uint32]chan dialResult),
	}
	go m.readLoop()
	return m
}

func (m *Mux) SetServerDialer(dial func(ctx context.Context, addr string) (net.Conn, error)) {
	m.serverDial = dial
}

func (m *Mux) Close() error {
	if m.closed.Swap(true) {
		return net.ErrClosed
	}
	_ = m.transport.Close()
	m.closeAll(net.ErrClosed)
	return nil
}

func (m *Mux) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	if m.closed.Load() {
		return nil, net.ErrClosed
	}
	id := m.nextID.Add(1)
	resCh := make(chan dialResult, 1)

	m.mu.Lock()
	m.pending[id] = resCh
	m.mu.Unlock()

	if err := m.sendFrame(frameOpen, id, []byte(addr)); err != nil {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, err
	}

	select {
	case res := <-resCh:
		return res.conn, res.err
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		_ = m.sendFrame(frameClose, id, nil)
		return nil, ctx.Err()
	}
}

func (m *Mux) readLoop() {
	for {
		msg, err := m.transport.Read(time.Time{})
		if err != nil {
			m.closeAll(err)
			return
		}
		if len(msg) < frameHeaderLen {
			continue
		}
		typ := msg[0]
		id := binary.BigEndian.Uint32(msg[1:5])
		n := int(binary.BigEndian.Uint32(msg[5:9]))
		if n < 0 || frameHeaderLen+n > len(msg) {
			continue
		}
		payload := msg[frameHeaderLen : frameHeaderLen+n]

		switch typ {
		case frameOpen:
			addr := string(payload)
			if m.serverDial == nil {
				_ = m.sendFrame(frameOpenErr, id, []byte("no server dialer"))
				continue
			}
			go m.handleOpen(id, addr)
		case frameOpenOK:
			m.handleOpenOK(id)
		case frameOpenErr:
			m.handleOpenErr(id, string(payload))
		case frameData:
			m.handleData(id, payload)
		case frameClose:
			m.handleClose(id)
		}
	}
}

func (m *Mux) handleOpen(id uint32, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := m.serverDial(ctx, addr)
	if err != nil {
		_ = m.sendFrame(frameOpenErr, id, []byte(err.Error()))
		return
	}
	s := m.newStream(id)
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
	if err := m.sendFrame(frameOpenOK, id, nil); err != nil {
		s.closeRemote()
		_ = conn.Close()
		return
	}
	go bridgeStreams(s, conn)
}

func (m *Mux) handleOpenOK(id uint32) {
	m.mu.Lock()
	resCh, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	s := m.newStream(id)
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
	resCh <- dialResult{conn: s}
	close(resCh)
}

func (m *Mux) handleOpenErr(id uint32, msg string) {
	m.mu.Lock()
	resCh, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	resCh <- dialResult{err: fmt.Errorf("dial error: %s", msg)}
	close(resCh)
}

func (m *Mux) handleData(id uint32, payload []byte) {
	m.mu.Lock()
	s := m.streams[id]
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.push(payload)
}

func (m *Mux) handleClose(id uint32) {
	m.mu.Lock()
	s := m.streams[id]
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.closeRemote()
}

func (m *Mux) sendFrame(typ byte, id uint32, payload []byte) error {
	if m.closed.Load() {
		return net.ErrClosed
	}
	if payload == nil {
		payload = []byte{}
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], id)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(payload)))
	copy(buf[9:], payload)
	return m.transport.Write(buf)
}

func (m *Mux) closeAll(err error) {
	m.mu.Lock()
	pending := m.pending
	streams := m.streams
	m.pending = make(map[uint32]chan dialResult)
	m.streams = make(map[uint32]*stream)
	m.mu.Unlock()

	for _, ch := range pending {
		ch <- dialResult{err: err}
		close(ch)
	}
	for _, s := range streams {
		s.closeRemote()
	}
}

type stream struct {
	id        uint32
	mux       *Mux
	closeOnce sync.Once
	closed    atomic.Bool

	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer
}

func (m *Mux) newStream(id uint32) *stream {
	s := &stream{
		id:  id,
		mux: m,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *stream) push(p []byte) {
	if s.closed.Load() {
		return
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return
	}
	_, _ = s.buf.Write(p)
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.buf.Len() == 0 && !s.closed.Load() {
		s.cond.Wait()
	}
	if s.buf.Len() == 0 && s.closed.Load() {
		return 0, io.EOF
	}
	return s.buf.Read(p)
}

func (s *stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFramePayload {
			n = maxFramePayload
		}
		if err := s.mux.sendFrame(frameData, s.id, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
		s.mux.mu.Lock()
		delete(s.mux.streams, s.id)
		s.mux.mu.Unlock()
		_ = s.mux.sendFrame(frameClose, s.id, nil)
	})
	return nil
}

func (s *stream) closeRemote() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
		s.mux.mu.Lock()
		delete(s.mux.streams, s.id)
		s.mux.mu.Unlock()
	})
}

func (s *stream) LocalAddr() net.Addr  { return dummyAddr{} }
func (s *stream) RemoteAddr() net.Addr { return dummyAddr{} }
func (s *stream) SetDeadline(t time.Time) error {
	return nil
}
func (s *stream) SetReadDeadline(t time.Time) error {
	return nil
}
func (s *stream) SetWriteDeadline(t time.Time) error {
	return nil
}

func bridgeStreams(a io.ReadWriteCloser, b net.Conn) {
	var once sync.Once
	closeAll := func() {
		_ = a.Close()
		_ = b.Close()
	}

	go func() {
		_, _ = io.Copy(b, a)
		once.Do(closeAll)
	}()
	go func() {
		_, _ = io.Copy(a, b)
		once.Do(closeAll)
	}()
}
