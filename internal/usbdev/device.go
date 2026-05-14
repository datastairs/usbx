// Package usbdev defines virtual USB device descriptors and endpoint configuration
// for the emulated USB channel.
package usbdev

import (
	"encoding/binary"
)

// USB descriptor types.
const (
	DescTypeDevice        = 1
	DescTypeConfiguration = 2
)

// Standard USB constants for a vendor-specific device.
const (
	VendorID  = 0x5553 // "US"
	ProductID = 0x4258 // "BX"

	// Endpoint addresses.
	Ep0Control  = 0x00 // Control endpoint
	Ep1BulkIn   = 0x81 // Bulk IN  (B→A, internet responses)
	Ep2BulkOut  = 0x02 // Bulk OUT (A→B, proxy requests)

	MaxPacketSize = 512 // Bulk endpoint max packet size (high-speed)
)

// DeviceDescriptor is a standard USB device descriptor (18 bytes).
type DeviceDescriptor struct {
	Length            uint8
	DescriptorType    uint8
	USBSpec           uint16 // BCD USB
	DeviceClass       uint8
	DeviceSubClass    uint8
	DeviceProtocol    uint8
	MaxPacketSize0    uint8
	IDVendor          uint16
	IDProduct         uint16
	BCDDevice         uint16
	ManufacturerIndex uint8
	ProductIndex      uint8
	SerialIndex       uint8
	NumConfigurations uint8
}

// ConfigDescriptor is a USB configuration descriptor.
type ConfigDescriptor struct {
	// Config descriptor (9 bytes)
	Length             uint8
	DescriptorType     uint8
	TotalLength        uint16
	NumInterfaces      uint8
	ConfigurationValue uint8
	ConfigurationIndex uint8
	Attributes         uint8
	MaxPower           uint8

	// Interface descriptor (9 bytes)
	InterfaceLength       uint8
	InterfaceDescType     uint8
	InterfaceNumber       uint8
	AlternateSetting      uint8
	NumEndpoints          uint8
	InterfaceClass        uint8
	InterfaceSubClass     uint8
	InterfaceProtocol     uint8
	InterfaceStringIndex  uint8

	// Endpoint 1: Bulk IN
	Ep1Length          uint8
	Ep1DescType        uint8
	Ep1Address         uint8 // 0x81
	Ep1Attributes      uint8 // Bulk
	Ep1MaxPacketSize   uint16
	Ep1Interval        uint8

	// Endpoint 2: Bulk OUT
	Ep2Length          uint8
	Ep2DescType        uint8
	Ep2Address         uint8 // 0x02
	Ep2Attributes      uint8 // Bulk
	Ep2MaxPacketSize   uint16
	Ep2Interval        uint8
}

// NewDeviceDescriptor creates a standard virtual USB device descriptor.
func NewDeviceDescriptor() DeviceDescriptor {
	return DeviceDescriptor{
		Length:            18,
		DescriptorType:    DescTypeDevice,
		USBSpec:           0x0200, // USB 2.0
		DeviceClass:       0xFF,   // Vendor-specific
		DeviceSubClass:    0x00,
		DeviceProtocol:    0x00,
		MaxPacketSize0:    64,
		IDVendor:          VendorID,
		IDProduct:         ProductID,
		BCDDevice:         0x0100,
		ManufacturerIndex: 0,
		ProductIndex:      0,
		SerialIndex:       0,
		NumConfigurations: 1,
	}
}

// NewConfigDescriptor creates a standard configuration with 2 bulk endpoints.
func NewConfigDescriptor() ConfigDescriptor {
	return ConfigDescriptor{
		Length:             9,
		DescriptorType:     DescTypeConfiguration,
		TotalLength:        9 + 9 + 7 + 7, // config + interface + 2 endpoints
		NumInterfaces:      1,
		ConfigurationValue: 1,
		ConfigurationIndex: 0,
		Attributes:         0x80, // Bus-powered
		MaxPower:           50,   // 100mA

		InterfaceLength:      9,
		InterfaceDescType:    4, // INTERFACE descriptor
		InterfaceNumber:      0,
		AlternateSetting:     0,
		NumEndpoints:         2,
		InterfaceClass:       0xFF, // Vendor-specific
		InterfaceSubClass:    0x00,
		InterfaceProtocol:    0x00,
		InterfaceStringIndex: 0,

		Ep1Length:        7,
		Ep1DescType:      5, // ENDPOINT descriptor
		Ep1Address:       Ep1BulkIn,
		Ep1Attributes:    0x02, // Bulk
		Ep1MaxPacketSize: MaxPacketSize,
		Ep1Interval:      0,

		Ep2Length:        7,
		Ep2DescType:      5, // ENDPOINT descriptor
		Ep2Address:       Ep2BulkOut,
		Ep2Attributes:    0x02, // Bulk
		Ep2MaxPacketSize: MaxPacketSize,
		Ep2Interval:      0,
	}
}

// Encode serializes a DeviceDescriptor to bytes.
func (d *DeviceDescriptor) Encode() []byte {
	buf := make([]byte, 18)
	buf[0] = d.Length
	buf[1] = d.DescriptorType
	binary.LittleEndian.PutUint16(buf[2:4], d.USBSpec)
	buf[4] = d.DeviceClass
	buf[5] = d.DeviceSubClass
	buf[6] = d.DeviceProtocol
	buf[7] = d.MaxPacketSize0
	binary.LittleEndian.PutUint16(buf[8:10], d.IDVendor)
	binary.LittleEndian.PutUint16(buf[10:12], d.IDProduct)
	binary.LittleEndian.PutUint16(buf[12:14], d.BCDDevice)
	buf[14] = d.ManufacturerIndex
	buf[15] = d.ProductIndex
	buf[16] = d.SerialIndex
	buf[17] = d.NumConfigurations
	return buf
}

// Encode serializes the full configuration descriptor (config + interface + 2 endpoints).
func (c *ConfigDescriptor) Encode() []byte {
	total := int(c.TotalLength)
	buf := make([]byte, total)

	// Config descriptor (9 bytes)
	buf[0] = c.Length
	buf[1] = c.DescriptorType
	binary.LittleEndian.PutUint16(buf[2:4], c.TotalLength)
	buf[4] = c.NumInterfaces
	buf[5] = c.ConfigurationValue
	buf[6] = c.ConfigurationIndex
	buf[7] = c.Attributes
	buf[8] = c.MaxPower

	// Interface descriptor (9 bytes) at offset 9
	off := 9
	buf[off+0] = c.InterfaceLength
	buf[off+1] = c.InterfaceDescType
	buf[off+2] = c.InterfaceNumber
	buf[off+3] = c.AlternateSetting
	buf[off+4] = c.NumEndpoints
	buf[off+5] = c.InterfaceClass
	buf[off+6] = c.InterfaceSubClass
	buf[off+7] = c.InterfaceProtocol
	buf[off+8] = c.InterfaceStringIndex

	// Endpoint 1: Bulk IN (7 bytes) at offset 18
	off = 18
	buf[off+0] = c.Ep1Length
	buf[off+1] = c.Ep1DescType
	buf[off+2] = c.Ep1Address
	buf[off+3] = c.Ep1Attributes
	binary.LittleEndian.PutUint16(buf[off+4:off+6], c.Ep1MaxPacketSize)
	buf[off+6] = c.Ep1Interval

	// Endpoint 2: Bulk OUT (7 bytes) at offset 25
	off = 25
	buf[off+0] = c.Ep2Length
	buf[off+1] = c.Ep2DescType
	buf[off+2] = c.Ep2Address
	buf[off+3] = c.Ep2Attributes
	binary.LittleEndian.PutUint16(buf[off+4:off+6], c.Ep2MaxPacketSize)
	buf[off+6] = c.Ep2Interval

	return buf
}
