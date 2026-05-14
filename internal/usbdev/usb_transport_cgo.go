//go:build cgo

package usbdev

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"usbx/internal/protocol"

	"github.com/google/gousb"
)

// usbTransport implements Transport using raw USB bulk endpoint I/O.
// It communicates through a USB device (physical or virtual) identified by VID/PID.
//
// Endpoint mapping:
//
//	EP1 (0x81) Bulk IN  → remote→local data (internet responses when on side-a)
//	EP2 (0x02) Bulk OUT → local→remote data (proxy requests when on side-a)
//
// USB communication model (VM passthrough scenario):
//
//	Machine A (host OS)           USB Device              Machine B (VM guest)
//	    │                                                      │
//	    │──── write EP2 OUT ────→ [device buffers] ──→ read EP2 OUT ────│
//	    │                                                      │
//	    │←─── read EP1 IN ────── [device buffers] ←── write EP1 IN ────│
//	    │                                                      │
//	The USB device must be a programmable gadget (e.g. Raspberry Pi Pico)
//	that acts as a bidirectional buffer between the two hosts.
type usbTransport struct {
	ctx    *gousb.Context
	dev    *gousb.Device
	intf   *gousb.Interface
	epIn   *gousb.InEndpoint  // EP1 Bulk IN (0x81)
	epOut  *gousb.OutEndpoint // EP2 Bulk OUT (0x02)

	vid, pid uint16

	state    atomic.Int32
	writeMu  sync.Mutex

	closeOnce sync.Once
	closeCh   chan struct{}

	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
	rxFrames atomic.Uint64
	txFrames atomic.Uint64

	buf []byte
}

// NewUSBTransport opens a USB device and returns a Transport that communicates
// via bulk endpoints. Only available when built with cgo + libusb.
func NewUSBTransport(cfg USBTransportConfig) (Transport, error) {
	if cfg.VID == 0 {
		cfg.VID = VendorID
	}
	if cfg.PID == 0 {
		cfg.PID = ProductID
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	ctx := gousb.NewContext()
	defer func() {
		if ctx != nil {
			ctx.Close()
		}
	}()

	dev, err := ctx.OpenDeviceWithVIDPID(gousb.ID(cfg.VID), gousb.ID(cfg.PID))
	if err != nil {
		return nil, fmt.Errorf("usbdev: open device %04x:%04x: %w", cfg.VID, cfg.PID, err)
	}
	if dev == nil {
		return nil, fmt.Errorf("usbdev: device %04x:%04x not found", cfg.VID, cfg.PID)
	}

	// Detach kernel driver if any, so we can claim the interface.
	dev.SetAutoDetach(true)

	cfgNum, err := dev.ActiveConfigNum()
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("usbdev: get active config: %w", err)
	}

	intf, epIn, epOut, err := claimEndpoints(dev, cfgNum)
	if err != nil {
		dev.Close()
		return nil, err
	}

	t := &usbTransport{
		ctx:     ctx,
		dev:     dev,
		intf:    intf,
		epIn:    epIn,
		epOut:   epOut,
		vid:     cfg.VID,
		pid:     cfg.PID,
		closeCh: make(chan struct{}),
		buf:     make([]byte, protocol.MaxPayloadSize+protocol.HeaderSize),
	}

	ctx = nil // Prevent defer from closing ctx; Transport owns it now.
	t.state.Store(int32(StateAttached))

	log.Printf("[usbdev] USB transport opened: VID=%04x PID=%04x", cfg.VID, cfg.PID)
	return t, nil
}

func claimEndpoints(dev *gousb.Device, cfgNum int) (*gousb.Interface, *gousb.InEndpoint, *gousb.OutEndpoint, error) {
	intf, done, err := dev.DefaultInterface()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("usbdev: claim interface: %w", err)
	}
	defer done()

	var epIn *gousb.InEndpoint
	var epOut *gousb.OutEndpoint

	for _, desc := range intf.Setting.Endpoints {
		switch desc.Address {
		case Ep1BulkIn:
			ep, err := intf.InEndpoint(int(desc.Address & 0x0F))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("usbdev: open IN ep 0x%02x: %w", Ep1BulkIn, err)
			}
			epIn = ep
		case Ep2BulkOut:
			ep, err := intf.OutEndpoint(int(desc.Address & 0x0F))
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("usbdev: open OUT ep 0x%02x: %w", Ep2BulkOut, err)
			}
			epOut = ep
		}
	}

	if epIn == nil {
		return nil, nil, nil, fmt.Errorf("usbdev: IN endpoint 0x%02x not found", Ep1BulkIn)
	}
	if epOut == nil {
		return nil, nil, nil, fmt.Errorf("usbdev: OUT endpoint 0x%02x not found", Ep2BulkOut)
	}

	return intf, epIn, epOut, nil
}

func (t *usbTransport) State() USBState {
	return USBState(t.state.Load())
}

func (t *usbTransport) SetState(s USBState) {
	t.state.Store(int32(s))
}

func (t *usbTransport) IsConfigured() bool {
	return t.State() == StateConfigured
}

// WriteFrame sends a protocol frame to the USB OUT endpoint.
func (t *usbTransport) WriteFrame(f *protocol.Frame) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// Encode frame into buffer.
	header := t.buf[:protocol.HeaderSize]
	protocol.EncodeInto(header, f)

	totalLen := protocol.HeaderSize + len(f.Payload)
	data := t.buf[:totalLen]
	if len(f.Payload) > 0 {
		copy(data[protocol.HeaderSize:], f.Payload)
	}

	n, err := t.epOut.Write(data)
	if err != nil {
		return fmt.Errorf("usbdev: write OUT: %w", err)
	}
	if n < totalLen {
		return fmt.Errorf("usbdev: short write OUT: %d/%d", n, totalLen)
	}

	t.txBytes.Add(uint64(totalLen))
	t.txFrames.Add(1)
	return nil
}

// ReadFrame reads a protocol frame from the USB IN endpoint.
func (t *usbTransport) ReadFrame() (*protocol.Frame, error) {
	n, err := t.epIn.Read(t.buf)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, gousb.TransferTimeout) {
			return nil, err
		}
		return nil, fmt.Errorf("usbdev: read IN: %w", err)
	}

	if n < protocol.HeaderSize {
		return nil, fmt.Errorf("usbdev: short read IN: %d bytes", n)
	}

	f := protocol.AcquireFrame()
	if err := protocol.DecodeFrom(f, t.buf[:n]); err != nil {
		protocol.ReleaseFrame(f)
		return nil, err
	}

	t.rxBytes.Add(uint64(n))
	t.rxFrames.Add(1)
	return f, nil
}

func (t *usbTransport) Stats() (uint64, uint64, uint64, uint64) {
	return t.rxBytes.Load(), t.txBytes.Load(), t.rxFrames.Load(), t.txFrames.Load()
}

func (t *usbTransport) Close() error {
	t.closeOnce.Do(func() {
		t.state.Store(int32(StateDetached))
		close(t.closeCh)
	})
	if t.intf != nil {
		t.intf.Close()
	}
	if t.dev != nil {
		t.dev.Close()
	}
	if t.ctx != nil {
		t.ctx.Close()
	}
	return nil
}

type usbAddr string

func (a usbAddr) Network() string  { return "usb" }
func (a usbAddr) String() string   { return string(a) }

func (t *usbTransport) LocalAddr() net.Addr {
	return usbAddr(fmt.Sprintf("usb:%04x:%04x", t.vid, t.pid))
}
