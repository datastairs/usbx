// Package protocol defines the USB-simulated wire protocol frame format.
// All multi-byte fields are big-endian.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	Magic   uint32 = 0x55534258 // "USBX"
	Version byte   = 0x01
	HeaderSize    = 16

	MaxPayloadSize = 65535
)

// Frame types.
const (
	TypeDeviceDescReq  byte = 0x01
	TypeDeviceDescResp byte = 0x02
	TypeConfigDescReq  byte = 0x03
	TypeConfigDescResp byte = 0x04

	TypeStreamOpen    byte = 0x10
	TypeStreamOpenAck byte = 0x11
	TypeStreamData    byte = 0x12
	TypeStreamClose   byte = 0x13
	TypeStreamCloseAck byte = 0x14
	TypeStreamError   byte = 0x15

	TypePing byte = 0xFE
	TypePong byte = 0xFF
)

// Frame represents a single protocol frame.
type Frame struct {
	Version  byte
	Type     byte
	Flags    uint16
	StreamID uint32
	Payload  []byte
}

var framePool = sync.Pool{
	New: func() any {
		return &Frame{}
	},
}

var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MaxPayloadSize)
		return &buf
	},
}

// AcquireFrame gets a Frame from the pool.
func AcquireFrame() *Frame {
	return framePool.Get().(*Frame)
}

// ReleaseFrame returns a Frame to the pool.
func ReleaseFrame(f *Frame) {
	f.Version = 0
	f.Type = 0
	f.Flags = 0
	f.StreamID = 0
	f.Payload = nil
	framePool.Put(f)
}

// AcquireBuffer gets a buffer from the pool.
func AcquireBuffer() []byte {
	bp := bufferPool.Get().(*[]byte)
	return *bp
}

// ReleaseBuffer returns a buffer to the pool.
func ReleaseBuffer(buf []byte) {
	if cap(buf) == MaxPayloadSize {
		bufferPool.Put(&buf)
	}
}

var (
	ErrInvalidMagic   = errors.New("protocol: invalid magic number")
	ErrInvalidVersion = errors.New("protocol: unsupported version")
	ErrPayloadTooBig  = errors.New("protocol: payload exceeds max size")
	ErrShortRead      = errors.New("protocol: short read")
)

// Encode writes the frame's header and payload to w.
// It does not pool-allocate; the caller owns the frame.
func (f *Frame) Encode(w io.Writer) error {
	if len(f.Payload) > MaxPayloadSize {
		return ErrPayloadTooBig
	}

	header := AcquireBuffer()[:HeaderSize]
	defer ReleaseBuffer(header)

	binary.BigEndian.PutUint32(header[0:4], Magic)
	header[4] = f.Version
	header[5] = f.Type
	binary.BigEndian.PutUint16(header[6:8], f.Flags)
	binary.BigEndian.PutUint32(header[8:12], f.StreamID)
	binary.BigEndian.PutUint32(header[12:16], uint32(len(f.Payload)))

	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("protocol: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("protocol: write payload: %w", err)
		}
	}
	return nil
}

// Decode reads a frame header and payload from r into f.
// The payload slice is a sub-slice of the provided buf if it fits,
// otherwise a new slice is allocated.
func (f *Frame) Decode(r io.Reader, buf []byte) error {
	if len(buf) < HeaderSize {
		return ErrShortRead
	}

	header := buf[:HeaderSize]
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("protocol: read header: %w", err)
	}

	magic := binary.BigEndian.Uint32(header[0:4])
	if magic != Magic {
		return ErrInvalidMagic
	}

	f.Version = header[4]
	if f.Version != Version {
		return ErrInvalidVersion
	}

	f.Type = header[5]
	f.Flags = binary.BigEndian.Uint16(header[6:8])
	f.StreamID = binary.BigEndian.Uint32(header[8:12])
	payloadLen := binary.BigEndian.Uint32(header[12:16])

	if payloadLen > MaxPayloadSize {
		return ErrPayloadTooBig
	}

	if payloadLen == 0 {
		f.Payload = nil
		return nil
	}

	if int(payloadLen) <= len(buf)-HeaderSize {
		f.Payload = buf[HeaderSize : HeaderSize+int(payloadLen)]
	} else {
		f.Payload = make([]byte, payloadLen)
	}

	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return fmt.Errorf("protocol: read payload: %w", err)
	}

	return nil
}

// ReadFrame reads a single frame from r using a pooled buffer.
// The returned frame must be released with ReleaseFrame.
// The frame's Payload is NOT a sub-slice of the pool buffer;
// callers should copy if they need to retain the payload.
func ReadFrame(r io.Reader) (*Frame, error) {
	buf := AcquireBuffer()
	f := AcquireFrame()

	if err := f.Decode(r, buf); err != nil {
		ReleaseFrame(f)
		ReleaseBuffer(buf)
		return nil, err
	}
	return f, nil
}

// EncodeInto writes the frame header and payload into dst.
// dst must be at least HeaderSize + len(f.Payload) bytes.
// Returns the number of bytes written.
func EncodeInto(dst []byte, f *Frame) int {
	binary.BigEndian.PutUint32(dst[0:4], Magic)
	dst[4] = f.Version
	dst[5] = f.Type
	binary.BigEndian.PutUint16(dst[6:8], f.Flags)
	binary.BigEndian.PutUint32(dst[8:12], f.StreamID)
	binary.BigEndian.PutUint32(dst[12:16], uint32(len(f.Payload)))
	if len(f.Payload) > 0 {
		copy(dst[HeaderSize:], f.Payload)
	}
	return HeaderSize + len(f.Payload)
}

// DecodeFrom decodes a frame from src into f.
// f.Payload will be a sub-slice of src (no copy).
func DecodeFrom(f *Frame, src []byte) error {
	if len(src) < HeaderSize {
		return ErrShortRead
	}

	magic := binary.BigEndian.Uint32(src[0:4])
	if magic != Magic {
		return ErrInvalidMagic
	}

	f.Version = src[4]
	if f.Version != Version {
		return ErrInvalidVersion
	}

	f.Type = src[5]
	f.Flags = binary.BigEndian.Uint16(src[6:8])
	f.StreamID = binary.BigEndian.Uint32(src[8:12])
	payloadLen := binary.BigEndian.Uint32(src[12:16])

	if payloadLen > MaxPayloadSize {
		return ErrPayloadTooBig
	}

	total := HeaderSize + int(payloadLen)
	if len(src) < total {
		return ErrShortRead
	}

	if payloadLen > 0 {
		f.Payload = src[HeaderSize:total]
	} else {
		f.Payload = nil
	}
	return nil
}

// WriteFrame encodes and writes f to w.
func WriteFrame(w io.Writer, f *Frame) error {
	return f.Encode(w)
}

// NewStreamOpen creates a STREAM_OPEN frame with target address as payload.
func NewStreamOpen(streamID uint32, addr string) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamOpen
	f.StreamID = streamID
	f.Payload = []byte(addr)
	return f
}

// NewStreamOpenAck creates a STREAM_OPEN_ACK frame.
func NewStreamOpenAck(streamID uint32) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamOpenAck
	f.StreamID = streamID
	return f
}

// NewStreamData creates a STREAM_DATA frame.
func NewStreamData(streamID uint32, data []byte) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamData
	f.StreamID = streamID
	f.Payload = data
	return f
}

// NewStreamClose creates a STREAM_CLOSE frame.
func NewStreamClose(streamID uint32) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamClose
	f.StreamID = streamID
	return f
}

// NewStreamCloseAck creates a STREAM_CLOSE_ACK frame.
func NewStreamCloseAck(streamID uint32) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamCloseAck
	f.StreamID = streamID
	return f
}

// NewStreamError creates a STREAM_ERROR frame with an error message.
func NewStreamError(streamID uint32, errMsg string) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeStreamError
	f.StreamID = streamID
	f.Payload = []byte(errMsg)
	return f
}

// NewDeviceDescReq creates a device descriptor request frame.
func NewDeviceDescReq() *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeDeviceDescReq
	return f
}

// NewDeviceDescResp creates a device descriptor response frame.
func NewDeviceDescResp(desc []byte) *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypeDeviceDescResp
	f.Payload = desc
	return f
}

// NewPing creates a keepalive ping frame.
func NewPing() *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypePing
	return f
}

// NewPong creates a keepalive pong frame.
func NewPong() *Frame {
	f := AcquireFrame()
	f.Version = Version
	f.Type = TypePong
	return f
}
