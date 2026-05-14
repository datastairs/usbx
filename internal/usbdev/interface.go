// Package usbdev defines the virtual USB device abstraction.
// It provides a Transport interface that abstracts the underlying communication
// channel (TCP for dev/test, raw USB endpoints for production).
package usbdev

import (
	"net"
	"time"

	"usbx/internal/protocol"
)

// USBState represents the state of the virtual USB device.
type USBState int32

const (
	StateDetached   USBState = iota
	StateAttached
	StateConfigured
)

// USBTransportConfig holds USB device connection parameters.
type USBTransportConfig struct {
	VID     uint16
	PID     uint16
	Timeout time.Duration
}

// Transport is the abstract interface for USB-simulated communication.
// Implementations include TCP (for dev/test) and raw USB endpoints via gousb.
type Transport interface {
	ReadFrame() (*protocol.Frame, error)
	WriteFrame(f *protocol.Frame) error
	Close() error
	State() USBState
	SetState(s USBState)
	Stats() (rxBytes, txBytes, rxFrames, txFrames uint64)
	LocalAddr() net.Addr
	IsConfigured() bool
}
