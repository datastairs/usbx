// Package forwarder implements the B-side internet forwarder.
// It receives stream open requests from the USB channel, dials the target,
// and relays data bidirectionally between the USB stream and the target.
package forwarder

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"usbx/internal/channel"
	"usbx/internal/usbdev"
)

// Forwarder handles forwarding USB streams to internet targets.
type Forwarder struct {
	mux    *channel.Mux
	dialer net.Dialer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Forwarder over a USB transport.
func New(transport usbdev.Transport) *Forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{
		mux: channel.NewMux(transport),
		dialer: net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}
	return f
}

// Start begins accepting streams and forwarding them.
func (f *Forwarder) Start() error {
	log.Printf("[forwarder] starting on %s", f.mux.Addr())

	for {
		stream, err := f.mux.Accept()
		if err != nil {
			return fmt.Errorf("forwarder: accept: %w", err)
		}

		f.wg.Add(1)
		go f.handleStream(stream)
	}
}

// Shutdown stops the forwarder gracefully.
func (f *Forwarder) Shutdown() {
	f.cancel()
	f.mux.Close()
	f.wg.Wait()
}

func (f *Forwarder) handleStream(s *channel.Stream) {
	defer f.wg.Done()

	target := s.TargetAddr()
	log.Printf("[forwarder] connecting to %s", target)

	targetConn, err := f.dialer.DialContext(f.ctx, "tcp", target)
	if err != nil {
		log.Printf("[forwarder] dial %s failed: %v", target, err)
		f.mux.RejectStream(s.StreamID(), err.Error())
		return
	}
	defer targetConn.Close()

	// Acknowledge the stream to side-a.
	if err := f.mux.AcceptStream(s.StreamID()); err != nil {
		log.Printf("[forwarder] accept stream %d failed: %v", s.StreamID(), err)
		return
	}

	log.Printf("[forwarder] connected to %s, relaying", target)

	var wg sync.WaitGroup
	wg.Add(2)

	// A→B: read from stream (side-a data), write to target (internet).
	go func() {
		defer wg.Done()
		io.Copy(targetConn, s)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// B→A: read from target (internet), write to stream (to side-a).
	go func() {
		defer wg.Done()
		io.Copy(s, targetConn)
		// Flush buffered writes so side-a gets all data before close.
		s.Flush()
	}()

	wg.Wait()
	// Both directions done. Close stream to send STREAM_CLOSE.
	// doFlush inside Close ensures any final buffered data is sent.
	s.Close()
}
