//go:build !linux

package usbdev

import "fmt"

// NewGadgetTransport is only available on Linux (requires FunctionFS).
// Use -transport tcp or -transport usb on non-Linux systems.
func NewGadgetTransport(cfg GadgetConfig) (Transport, error) {
	return nil, fmt.Errorf(
		"usbdev: gadget transport requires Linux (FunctionFS); " +
			"use -transport tcp for development mode",
	)
}
