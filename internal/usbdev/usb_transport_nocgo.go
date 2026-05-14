//go:build !cgo

package usbdev

import (
	"fmt"
)

// NewUSBTransport returns an error when built without cgo/libsub support.
// Install gcc + libusb and build with CGO_ENABLED=1 to enable USB transport.
func NewUSBTransport(cfg USBTransportConfig) (Transport, error) {
	return nil, fmt.Errorf(
		"usbdev: USB transport requires cgo + libusb (build with CGO_ENABLED=1 and install libusb); "+
			"use -transport tcp for development mode",
	)
}
