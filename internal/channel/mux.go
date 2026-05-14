// Package channel implements stream multiplexing over the USB transport.
// Multiple SOCKS5 connections are multiplexed onto a single USB channel
// using StreamID for demultiplexing.
//
// Optimized for high-latency (20-50ms) USB-over-network channels:
//   - Optimistic STREAM_OPEN: don't block on ACK, pipeline data immediately
//   - Write batching: buffer small writes, flush as large frames
//   - Auto-flush: timer-based flush ensures data doesn't stall
package channel

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"usbx/internal/protocol"
	"usbx/internal/usbdev"
)

const (
	// MaxBufferedWrite is the maximum bytes to buffer before auto-flushing.
	MaxBufferedWrite = 32768 // 32KB
	// FlushInterval is the max time to hold data before flushing.
	FlushInterval = 5 * time.Millisecond
	// WriteChCap is the capacity of the incoming data channel per stream.
	WriteChCap = 128
)

// Mux multiplexes multiple streams over a single USB transport.
type Mux struct {
	transport usbdev.Transport

	streams sync.Map
	nextID  atomic.Uint32

	acceptCh chan *Stream

	closeOnce sync.Once
	closeCh   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

// Stream represents a single multiplexed stream.
type Stream struct {
	id     uint32
	mux    *Mux
	closed atomic.Bool

	// Read path: data arriving from remote via handleFrame.
	readBuf []byte
	readMu  sync.Mutex
	writeCh chan []byte // populated by Mux.handleFrame for STREAM_DATA

	// ACK path: STREAM_OPEN_ACK (nil) or STREAM_ERROR (error).
	// nil means the stream opened successfully.
	// If using OpenStreamPipeline, read from this to detect async failure.
	ackCh   chan error
	ackOnce sync.Once // ensures ackCh is signaled at most once

	// Write buffering: client writes go to wb, flushed to transport.
	wb        []byte        // write buffer
	wbMu      sync.Mutex
	wbFlush   chan struct{} // signal to flush
	wbDone    chan struct{} // flush goroutine stopped

	ctx    context.Context
	cancel context.CancelFunc
}

// NewMux creates a new stream multiplexer over a USB transport.
func NewMux(transport usbdev.Transport) *Mux {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mux{
		transport: transport,
		acceptCh:  make(chan *Stream, 64),
		closeCh:   make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
	go m.readLoop()
	return m
}

// Accept returns the next incoming stream (STREAM_OPEN from remote).
func (m *Mux) Accept() (*Stream, error) {
	select {
	case s := <-m.acceptCh:
		return s, nil
	case <-m.closeCh:
		return nil, fmt.Errorf("channel: mux closed")
	}
}

// OpenStream creates a new outgoing stream and blocks until the remote
// side acknowledges (STREAM_OPEN_ACK) or rejects (STREAM_ERROR).
// For high-latency channels, prefer OpenStreamPipeline.
func (m *Mux) OpenStream(ctx context.Context, addr string) (*Stream, error) {
	s, err := m.OpenStreamPipeline(ctx, addr)
	if err != nil {
		return nil, err
	}

	// Wait for ACK or ERROR from remote.
	select {
	case ackErr := <-s.ackCh:
		if ackErr != nil {
			m.streams.Delete(s.id)
			s.Close()
			return nil, fmt.Errorf("channel: open stream rejected: %w", ackErr)
		}
		return s, nil
	case <-ctx.Done():
		m.streams.Delete(s.id)
		s.Close()
		return nil, ctx.Err()
	case <-m.closeCh:
		m.streams.Delete(s.id)
		s.Close()
		return nil, fmt.Errorf("channel: mux closed")
	}
}

// OpenStreamPipeline creates an outgoing stream WITHOUT waiting for
// the remote ACK. Data written to the stream is sent immediately;
// if the remote rejects the stream (STREAM_ERROR), subsequent reads
// will return an error. The caller can check AckCh() for async status.
//
// This saves one round-trip (20-50ms over USB-net) on the critical path.
func (m *Mux) OpenStreamPipeline(ctx context.Context, addr string) (*Stream, error) {
	id := m.nextID.Add(1)

	s := &Stream{
		id:       id,
		mux:      m,
		writeCh:  make(chan []byte, WriteChCap),
		ackCh:    make(chan error, 1),
		wbFlush:  make(chan struct{}, 1),
		wbDone:   make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	m.streams.Store(id, s)

	f := protocol.NewStreamOpen(id, addr)
	if err := m.transport.WriteFrame(f); err != nil {
		protocol.ReleaseFrame(f)
		m.streams.Delete(id)
		return nil, fmt.Errorf("channel: open stream: %w", err)
	}
	protocol.ReleaseFrame(f)

	// Start background flush goroutine for write batching.
	go s.flushLoop()

	return s, nil
}

// AcceptStream sends STREAM_OPEN_ACK to acknowledge an incoming stream.
func (m *Mux) AcceptStream(streamID uint32) error {
	f := protocol.NewStreamOpenAck(streamID)
	defer protocol.ReleaseFrame(f)
	return m.transport.WriteFrame(f)
}

// RejectStream sends STREAM_ERROR to reject an incoming stream.
func (m *Mux) RejectStream(streamID uint32, errMsg string) error {
	f := protocol.NewStreamError(streamID, errMsg)
	defer protocol.ReleaseFrame(f)
	return m.transport.WriteFrame(f)
}

// AckCh returns a channel that receives nil on ACK or error on ERROR.
// For streams created with OpenStreamPipeline, the caller can monitor
// this to detect async connection failures.
func (s *Stream) AckCh() <-chan error {
	return s.ackCh
}

// readLoop continuously reads frames from the transport and routes them.
func (m *Mux) readLoop() {
	defer func() {
		m.cancel()
		m.Close()
	}()

	for {
		f, err := m.transport.ReadFrame()
		if err != nil {
			if !isClosed(m.closeCh) {
				log.Printf("[mux] read error: %v", err)
			}
			return
		}
		m.handleFrame(f)
		protocol.ReleaseFrame(f)
	}
}

func (m *Mux) handleFrame(f *protocol.Frame) {
	switch f.Type {
	case protocol.TypeStreamOpen:
		addr := string(f.Payload)
		s := &Stream{
			id:       f.StreamID,
			mux:      m,
			writeCh:  make(chan []byte, WriteChCap),
			ackCh:    make(chan error, 1),
			wbFlush:  make(chan struct{}, 1),
			wbDone:   make(chan struct{}),
		}
		s.ctx, s.cancel = context.WithCancel(m.ctx)
		m.streams.Store(f.StreamID, s)

		s.readMu.Lock()
		s.readBuf = []byte(addr)
		s.readMu.Unlock()

		go s.flushLoop()

		select {
		case m.acceptCh <- s:
		case <-m.closeCh:
			s.Close()
		}

	case protocol.TypeStreamOpenAck:
		if v, ok := m.streams.Load(f.StreamID); ok {
			s := v.(*Stream)
			s.signalAck(nil)
		}

	case protocol.TypeStreamData:
		if v, ok := m.streams.Load(f.StreamID); ok {
			s := v.(*Stream)
			if !s.closed.Load() {
				select {
				case s.writeCh <- f.Payload:
				default:
					// writeCh full, drop frame (backpressure)
				}
			}
		}

	case protocol.TypeStreamClose:
		if v, ok := m.streams.Load(f.StreamID); ok {
			s := v.(*Stream)
			s.Close()
		}

	case protocol.TypeStreamCloseAck:
		if v, ok := m.streams.Load(f.StreamID); ok {
			s := v.(*Stream)
			s.Close()
		}

	case protocol.TypeStreamError:
		if v, ok := m.streams.Load(f.StreamID); ok {
			s := v.(*Stream)
			errMsg := string(f.Payload)
			log.Printf("[mux] stream %d error: %s", f.StreamID, errMsg)
			s.signalAck(fmt.Errorf("stream error: %s", errMsg))
			s.Close()
		}

	case protocol.TypePing:
		pong := protocol.NewPong()
		m.transport.WriteFrame(pong)
		protocol.ReleaseFrame(pong)

	case protocol.TypePong:
	}
}

// Close shuts down the mux and all streams.
func (m *Mux) Close() error {
	m.closeOnce.Do(func() {
		close(m.closeCh)
		m.streams.Range(func(key, value any) bool {
			value.(*Stream).Close()
			return true
		})
		m.transport.Close()
	})
	return nil
}

func (s *Stream) signalAck(err error) {
	s.ackOnce.Do(func() {
		select {
		case s.ackCh <- err:
		default:
		}
	})
}

// flushLoop is a background goroutine that flushes the write buffer
// either when signaled or on a timer.
func (s *Stream) flushLoop() {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	defer close(s.wbDone)

	for {
		select {
		case <-s.wbFlush:
			s.doFlush()
		case <-ticker.C:
			s.doFlush()
		case <-s.ctx.Done():
			s.doFlush()
			return
		}
	}
}

func (s *Stream) doFlush() {
	s.wbMu.Lock()
	if len(s.wb) == 0 {
		s.wbMu.Unlock()
		return
	}
	data := s.wb
	s.wb = nil
	s.wbMu.Unlock()

	s.writeFrame(protocol.NewStreamData(s.id, data))
}

// Write appends data to the stream's write buffer. The buffer is flushed
// when it reaches MaxBufferedWrite or after FlushInterval.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("channel: stream closed")
	}

	s.wbMu.Lock()
	s.wb = append(s.wb, p...)
	shouldFlush := len(s.wb) >= MaxBufferedWrite
	s.wbMu.Unlock()

	if shouldFlush {
		select {
		case s.wbFlush <- struct{}{}:
		default:
			// Flush already pending.
		}
	}
	return len(p), nil
}

// Flush forces immediate flush of buffered write data.
func (s *Stream) Flush() error {
	select {
	case s.wbFlush <- struct{}{}:
	default:
	}
	return nil
}

// writeFrame sends a single frame. For large payloads, splits into
// multiple MaxPayloadSize frames. This is used for direct writes
// that bypass the buffer (when buffer is flushed).
func (s *Stream) writeFrame(f *protocol.Frame) {
	if len(f.Payload) <= protocol.MaxPayloadSize {
		s.mux.transport.WriteFrame(f)
		protocol.ReleaseFrame(f)
		return
	}

	// Split large payload into multiple frames.
	data := f.Payload
	streamID := f.StreamID
	protocol.ReleaseFrame(f)

	for len(data) > 0 {
		chunk := data
		if len(chunk) > protocol.MaxPayloadSize {
			chunk = chunk[:protocol.MaxPayloadSize]
		}
		cf := protocol.NewStreamData(streamID, chunk)
		s.mux.transport.WriteFrame(cf)
		protocol.ReleaseFrame(cf)
		data = data[len(chunk):]
	}
}

// Read reads data from the stream into p.
func (s *Stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	if len(s.readBuf) > 0 {
		n := copy(p, s.readBuf)
		s.readBuf = s.readBuf[n:]
		s.readMu.Unlock()
		return n, nil
	}
	s.readMu.Unlock()

	for {
		select {
		case data, ok := <-s.writeCh:
			if !ok {
				return 0, io.EOF
			}
			n := copy(p, data)
			if n < len(data) {
				s.readMu.Lock()
				s.readBuf = append(s.readBuf, data[n:]...)
				s.readMu.Unlock()
			}
			return n, nil
		case <-s.ctx.Done():
			return 0, io.EOF
		case <-s.mux.closeCh:
			return 0, io.EOF
		}
	}
}

// Close closes the stream. Flushes pending writes first.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}

	s.cancel()

	// Flush remaining buffered data.
	s.doFlush()

	s.mux.streams.Delete(s.id)

	f := protocol.NewStreamClose(s.id)
	s.mux.transport.WriteFrame(f)
	protocol.ReleaseFrame(f)

	return nil
}

// TargetAddr returns the target address for incoming streams.
func (s *Stream) TargetAddr() string {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return string(s.readBuf)
}

// StreamID returns the stream's identifier.
func (s *Stream) StreamID() uint32 {
	return s.id
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

var _ io.ReadWriteCloser = (*Stream)(nil)

var _ interface {
	Accept() (*Stream, error)
	Close() error
	Addr() net.Addr
} = (*Mux)(nil)

// Addr returns the local address of the underlying transport.
func (m *Mux) Addr() net.Addr {
	return m.transport.LocalAddr()
}

// SetDeadline is a no-op for interface compatibility.
func (s *Stream) SetDeadline(t time.Time) error { return nil }

// SetReadDeadline is a no-op for interface compatibility.
func (s *Stream) SetReadDeadline(t time.Time) error { return nil }

// SetWriteDeadline is a no-op for interface compatibility.
func (s *Stream) SetWriteDeadline(t time.Time) error { return nil }
