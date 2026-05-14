//go:build linux

package usbdev

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"usbx/internal/protocol"
)

const (
	// FunctionFS event types.
	ffsBind    = 0
	ffsUnbind  = 1
	ffsEnable  = 2
	ffsDisable = 3
	ffsSetup   = 4
	ffsSuspend = 5
	ffsResume  = 6

	// FunctionFS descriptor header magic and flags (v2).
	ffsDescriptorsMagic = 1
	ffsHighSpeed        = 1

	// Default mount point for FunctionFS.
	defaultGadgetDir = "/dev/usb-ffs/usbx"
)

type gadgetTransport struct {
	ep0 *os.File // FunctionFS control endpoint

	epIn  *os.File // EP1 IN: write (gadget→host, A→B data)
	epOut *os.File // EP2 OUT: read (host→gadget, B→A data)

	gadgetDir string

	state    atomic.Int32
	writeMu  sync.Mutex

	closeOnce sync.Once
	closeCh   chan struct{}

	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
	rxFrames atomic.Uint64
	txFrames atomic.Uint64

	writeBuf []byte
}

// NewGadgetTransport creates a USB gadget transport via Linux FunctionFS.
// It expects the FunctionFS mount to already exist at cfg.GadgetDir
// (see scripts/setup-gadget.sh to create it).
//
// The transport creates a virtual USB device with VID=0x5553 PID=0x4258
// and two bulk endpoints (EP1 IN, EP2 OUT). The hypervisor can pass this
// gadget device through to a VM, where side-b connects via USB transport.
func NewGadgetTransport(cfg GadgetConfig) (Transport, error) {
	if cfg.GadgetDir == "" {
		cfg.GadgetDir = defaultGadgetDir
	}

	t := &gadgetTransport{
		gadgetDir: cfg.GadgetDir,
		closeCh:   make(chan struct{}),
		writeBuf:  make([]byte, protocol.MaxPayloadSize+protocol.HeaderSize),
	}

	if err := t.init(); err != nil {
		return nil, fmt.Errorf("usbdev: gadget init: %w", err)
	}

	t.state.Store(int32(StateAttached))
	log.Printf("[usbdev] gadget transport ready at %s", cfg.GadgetDir)
	return t, nil
}

func (t *gadgetTransport) init() error {
	// Open FunctionFS control endpoint.
	ep0Path := filepath.Join(t.gadgetDir, "ep0")
	ep0, err := os.OpenFile(ep0Path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w (is the FunctionFS gadget mounted?)", ep0Path, err)
	}
	t.ep0 = ep0

	// Write USB descriptors to ep0.
	if err := t.writeDescriptors(); err != nil {
		t.ep0.Close()
		return fmt.Errorf("write descriptors: %w", err)
	}

	// Wait for host to enable the gadget (FUNCTIONFS_ENABLE event).
	if err := t.waitEnable(); err != nil {
		t.ep0.Close()
		return fmt.Errorf("wait for enable: %w", err)
	}

	// Open bulk endpoints.
	ep1Path := filepath.Join(t.gadgetDir, "ep1")
	ep2Path := filepath.Join(t.gadgetDir, "ep2")

	epIn, err := os.OpenFile(ep1Path, os.O_RDWR, 0)
	if err != nil {
		t.ep0.Close()
		return fmt.Errorf("open %s: %w", ep1Path, err)
	}
	t.epIn = epIn

	epOut, err := os.OpenFile(ep2Path, os.O_RDWR, 0)
	if err != nil {
		t.ep0.Close()
		t.epIn.Close()
		return fmt.Errorf("open %s: %w", ep2Path, err)
	}
	t.epOut = epOut

	// Background ep0 event monitor for disconnect/reconnect.
	go t.ep0EventLoop()

	return nil
}

// writeDescriptors builds and writes the FFS v2 descriptor blob to ep0.
func (t *gadgetTransport) writeDescriptors() error {
	devDesc := NewDeviceDescriptor().Encode()
	cfgDesc := NewConfigDescriptor().Encode()

	// FFS v2 descriptor header:
	//   magic (4 bytes LE)
	//   length (4 bytes LE) — total descriptor bytes after this header
	//   flags (4 bytes LE) — bit 0 = high-speed capable
	hdr := make([]byte, 12)
	binary.LittleEndian.PutUint32(hdr[0:4], ffsDescriptorsMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(devDesc)+len(cfgDesc)))
	binary.LittleEndian.PutUint32(hdr[8:12], ffsHighSpeed)

	data := make([]byte, 0, 12+len(devDesc)+len(cfgDesc))
	data = append(data, hdr...)
	data = append(data, devDesc...)
	data = append(data, cfgDesc...)

	n, err := t.ep0.Write(data)
	if err != nil {
		return fmt.Errorf("write descriptors to ep0: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("short write to ep0: %d/%d", n, len(data))
	}

	return nil
}

// waitEnable reads ep0 events until FUNCTIONFS_ENABLE arrives.
func (t *gadgetTransport) waitEnable() error {
	event := make([]byte, 12)
	for {
		n, err := t.ep0.Read(event)
		if err != nil {
			return fmt.Errorf("read ep0 event: %w", err)
		}
		evType := event[8]
		switch evType {
		case ffsBind:
			log.Print("[usbdev] gadget: bind")
		case ffsEnable:
			log.Print("[usbdev] gadget: endpoints enabled")
			return nil
		case ffsUnbind:
			log.Print("[usbdev] gadget: unbind")
			return fmt.Errorf("gadget unbound before enable")
		case ffsDisable:
			log.Print("[usbdev] gadget: disable")
			return fmt.Errorf("gadget disabled before enable")
		case ffsSetup:
			// Control request from host — kernel handles these automatically.
		case ffsSuspend:
			log.Print("[usbdev] gadget: suspend")
		case ffsResume:
			log.Print("[usbdev] gadget: resume")
		}
		_ = n
	}
}

// ep0EventLoop monitors ep0 for suspend/resume/disable/enable events
// during normal operation. Runs in a background goroutine.
func (t *gadgetTransport) ep0EventLoop() {
	event := make([]byte, 12)
	for {
		_, err := t.ep0.Read(event)
		if err != nil {
			select {
			case <-t.closeCh:
				return
			default:
				log.Printf("[usbdev] gadget ep0 error: %v", err)
				return
			}
		}

		evType := event[8]
		switch evType {
		case ffsSuspend:
			log.Print("[usbdev] gadget: host suspended")
		case ffsResume:
			log.Print("[usbdev] gadget: host resumed")
		case ffsDisable:
			log.Print("[usbdev] gadget: endpoints disabled")
		case ffsEnable:
			log.Print("[usbdev] gadget: endpoints re-enabled")
		case ffsSetup:
			// Control requests, handled by kernel.
		}
	}
}

func (t *gadgetTransport) State() USBState     { return USBState(t.state.Load()) }
func (t *gadgetTransport) SetState(s USBState)  { t.state.Store(int32(s)) }
func (t *gadgetTransport) IsConfigured() bool   { return t.State() == StateConfigured }

// WriteFrame sends a frame to EP1 IN (gadget→host → VM B).
func (t *gadgetTransport) WriteFrame(f *protocol.Frame) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	totalLen := protocol.HeaderSize + len(f.Payload)
	buf := t.writeBuf[:totalLen]
	protocol.EncodeInto(buf, f)

	if err := t.writeAll(t.epIn, buf); err != nil {
		return fmt.Errorf("usbdev: gadget write ep1: %w", err)
	}

	t.txBytes.Add(uint64(totalLen))
	t.txFrames.Add(1)
	return nil
}

// ReadFrame reads a frame from EP2 OUT (host→gadget ← VM B).
func (t *gadgetTransport) ReadFrame() (*protocol.Frame, error) {
	buf := protocol.AcquireBuffer()
	defer protocol.ReleaseBuffer(buf)

	// Read raw data from EP2 OUT.
	// On USB, a bulk transfer reads one whole packet or times out.
	n, err := t.readWithDeadline(t.epOut, buf[:4096])
	if err != nil {
		if isClosedChan(t.closeCh) {
			return nil, fmt.Errorf("usbdev: gadget transport closed")
		}
		return nil, fmt.Errorf("usbdev: gadget read ep2: %w", err)
	}

	if n < protocol.HeaderSize {
		return nil, fmt.Errorf("usbdev: gadget short read: %d bytes", n)
	}

	f := protocol.AcquireFrame()
	if err := protocol.DecodeFrom(f, buf[:n]); err != nil {
		protocol.ReleaseFrame(f)
		return nil, err
	}

	// Copy payload since buf will be released.
	if len(f.Payload) > 0 {
		cp := make([]byte, len(f.Payload))
		copy(cp, f.Payload)
		f.Payload = cp
	}

	t.rxBytes.Add(uint64(n))
	t.rxFrames.Add(1)
	return f, nil
}

// writeAll writes the full buffer to f, handling short writes.
func (t *gadgetTransport) writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// readWithDeadline sets a short deadline and reads from f.
// USB bulk transfers are naturally bounded, and Linux FFS delivers them as-is.
func (t *gadgetTransport) readWithDeadline(f *os.File, buf []byte) (int, error) {
	// Use poll-based read with explicit deadline via goroutine + channel.
	// FFS endpoints don't support SetReadDeadline (they're not sockets).
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := f.Read(buf)
		ch <- result{n, err}
	}()

	timeout := 30 * time.Second
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(timeout):
		return 0, fmt.Errorf("read timeout after %v", timeout)
	case <-t.closeCh:
		return 0, fmt.Errorf("transport closed")
	}
}

func (t *gadgetTransport) Stats() (uint64, uint64, uint64, uint64) {
	return t.rxBytes.Load(), t.txBytes.Load(), t.rxFrames.Load(), t.txFrames.Load()
}

func (t *gadgetTransport) Close() error {
	t.closeOnce.Do(func() {
		t.state.Store(int32(StateDetached))
		close(t.closeCh)
	})

	var errs []error
	if t.epIn != nil {
		if err := t.epIn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if t.epOut != nil {
		if err := t.epOut.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if t.ep0 != nil {
		if err := t.ep0.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("usbdev: gadget close: %v", errs)
	}
	return nil
}

type gadgetAddr string

func (a gadgetAddr) Network() string { return "gadget" }
func (a gadgetAddr) String() string  { return string(a) }

func (t *gadgetTransport) LocalAddr() net.Addr {
	return gadgetAddr(t.gadgetDir)
}

// isClosedChan reports whether ch is closed.
func isClosedChan(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
