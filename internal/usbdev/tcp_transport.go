package usbdev

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"usbx/internal/protocol"
)

// tcpTransport provides USB-semantic communication over a TCP connection.
// Used for development and testing when no physical/virtual USB device is available.
type tcpTransport struct {
	conn  net.Conn
	state atomic.Int32

	writeMu sync.Mutex

	closeOnce sync.Once
	closeCh   chan struct{}

	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
	rxFrames atomic.Uint64
	txFrames atomic.Uint64
}

// NewTCPTransport creates a USB transport over a TCP connection (dev/test mode).
func NewTCPTransport(conn net.Conn) Transport {
	t := &tcpTransport{
		conn:    conn,
		closeCh: make(chan struct{}),
	}
	t.state.Store(int32(StateAttached))
	return t
}

func (t *tcpTransport) State() USBState {
	return USBState(t.state.Load())
}

func (t *tcpTransport) SetState(s USBState) {
	t.state.Store(int32(s))
}

func (t *tcpTransport) IsConfigured() bool {
	return t.State() == StateConfigured
}

func (t *tcpTransport) WriteFrame(f *protocol.Frame) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := f.Encode(t.conn); err != nil {
		return fmt.Errorf("usbdev: write frame: %w", err)
	}
	t.txBytes.Add(uint64(protocol.HeaderSize + len(f.Payload)))
	t.txFrames.Add(1)
	return nil
}

func (t *tcpTransport) ReadFrame() (*protocol.Frame, error) {
	buf := protocol.AcquireBuffer()
	f, err := protocol.ReadFrame(t.conn)
	if err != nil {
		protocol.ReleaseBuffer(buf)
		return nil, err
	}
	if len(f.Payload) > 0 {
		copied := make([]byte, len(f.Payload))
		copy(copied, f.Payload)
		f.Payload = copied
	}
	protocol.ReleaseBuffer(buf)
	t.rxBytes.Add(uint64(protocol.HeaderSize + len(f.Payload)))
	t.rxFrames.Add(1)
	return f, nil
}

func (t *tcpTransport) Stats() (uint64, uint64, uint64, uint64) {
	return t.rxBytes.Load(), t.txBytes.Load(), t.rxFrames.Load(), t.txFrames.Load()
}

func (t *tcpTransport) Close() error {
	t.closeOnce.Do(func() {
		t.state.Store(int32(StateDetached))
		close(t.closeCh)
	})
	return t.conn.Close()
}

func (t *tcpTransport) LocalAddr() net.Addr {
	return t.conn.LocalAddr()
}

// Keepalive sends periodic PING frames over the transport.
func Keepalive(tr Transport, ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ping := protocol.NewPing()
			if err := tr.WriteFrame(ping); err != nil {
				log.Printf("[usbdev] keepalive ping failed: %v", err)
				protocol.ReleaseFrame(ping)
				return
			}
			protocol.ReleaseFrame(ping)
		}
	}
}
