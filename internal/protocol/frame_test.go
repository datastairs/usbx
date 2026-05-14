package protocol

import (
	"bytes"
	"testing"
)

func TestFrameEncodeDecode(t *testing.T) {
	original := &Frame{
		Version:  Version,
		Type:     TypeStreamData,
		Flags:    0,
		StreamID: 42,
		Payload:  []byte("hello, world"),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded := &Frame{}
	readBuf := AcquireBuffer()
	defer ReleaseBuffer(readBuf)

	if err := decoded.Decode(&buf, readBuf); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Version != original.Version {
		t.Errorf("version: got %d, want %d", decoded.Version, original.Version)
	}
	if decoded.Type != original.Type {
		t.Errorf("type: got %d, want %d", decoded.Type, original.Type)
	}
	if decoded.StreamID != original.StreamID {
		t.Errorf("streamID: got %d, want %d", decoded.StreamID, original.StreamID)
	}
	if string(decoded.Payload) != string(original.Payload) {
		t.Errorf("payload: got %q, want %q", string(decoded.Payload), string(original.Payload))
	}
}

func TestFrameEmptyPayload(t *testing.T) {
	original := &Frame{
		Version:  Version,
		Type:     TypeStreamClose,
		StreamID: 1,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded := &Frame{}
	readBuf := AcquireBuffer()
	defer ReleaseBuffer(readBuf)

	if err := decoded.Decode(&buf, readBuf); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Payload != nil {
		t.Errorf("payload should be nil, got %q", decoded.Payload)
	}
}

func TestFrameLargePayload(t *testing.T) {
	largePayload := make([]byte, MaxPayloadSize)
	for i := range largePayload {
		largePayload[i] = byte(i % 256)
	}

	original := &Frame{
		Version:  Version,
		Type:     TypeStreamData,
		StreamID: 99,
		Payload:  largePayload,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded := &Frame{}
	readBuf := AcquireBuffer()
	defer ReleaseBuffer(readBuf)

	if err := decoded.Decode(&buf, readBuf); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded.Payload) != MaxPayloadSize {
		t.Errorf("payload length: got %d, want %d", len(decoded.Payload), MaxPayloadSize)
	}
	for i := range decoded.Payload {
		if decoded.Payload[i] != largePayload[i] {
			t.Errorf("payload[%d]: got %d, want %d", i, decoded.Payload[i], largePayload[i])
			break
		}
	}
}

func TestFrameOversizedPayload(t *testing.T) {
	original := &Frame{
		Version:  Version,
		Type:     TypeStreamData,
		StreamID: 1,
		Payload:  make([]byte, MaxPayloadSize+1),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != ErrPayloadTooBig {
		t.Errorf("expected ErrPayloadTooBig, got %v", err)
	}
}

func TestInvalidMagic(t *testing.T) {
	data := make([]byte, HeaderSize)
	data[4] = Version
	data[5] = TypeStreamData

	decoded := &Frame{}
	readBuf := AcquireBuffer()
	defer ReleaseBuffer(readBuf)

	err := decoded.Decode(bytes.NewReader(data), readBuf)
	if err != ErrInvalidMagic {
		t.Errorf("expected ErrInvalidMagic, got %v", err)
	}
}

func TestNewFrameConstructors(t *testing.T) {
	tests := []struct {
		name string
		f    *Frame
		typ  byte
	}{
		{"StreamOpen", NewStreamOpen(1, "example.com:80"), TypeStreamOpen},
		{"StreamOpenAck", NewStreamOpenAck(1), TypeStreamOpenAck},
		{"StreamData", NewStreamData(1, []byte("data")), TypeStreamData},
		{"StreamClose", NewStreamClose(1), TypeStreamClose},
		{"StreamCloseAck", NewStreamCloseAck(1), TypeStreamCloseAck},
		{"StreamError", NewStreamError(1, "error"), TypeStreamError},
		{"DeviceDescReq", NewDeviceDescReq(), TypeDeviceDescReq},
		{"Ping", NewPing(), TypePing},
		{"Pong", NewPong(), TypePong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.f.Type != tt.typ {
				t.Errorf("type: got %d, want %d", tt.f.Type, tt.typ)
			}
			if tt.f.Version != Version {
				t.Errorf("version: got %d, want %d", tt.f.Version, Version)
			}
			ReleaseFrame(tt.f)
		})
	}
}

func TestFramePool(t *testing.T) {
	f1 := AcquireFrame()
	f1.Type = TypeStreamData
	f1.StreamID = 7
	f1.Payload = []byte("pool test")
	ReleaseFrame(f1)

	f2 := AcquireFrame()
	if f2.Type != 0 || f2.StreamID != 0 {
		t.Error("pooled frame was not zeroed")
	}
	if f2.Payload != nil {
		t.Error("pooled frame payload was not nil")
	}
	ReleaseFrame(f2)
}

func BenchmarkFrameEncode(b *testing.B) {
	f := &Frame{
		Version:  Version,
		Type:     TypeStreamData,
		StreamID: 1,
		Payload:  make([]byte, 4096),
	}
	var buf bytes.Buffer

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		f.Encode(&buf)
	}
}

func BenchmarkFrameDecode(b *testing.B) {
	f := &Frame{
		Version:  Version,
		Type:     TypeStreamData,
		StreamID: 1,
		Payload:  make([]byte, 4096),
	}
	var buf bytes.Buffer
	f.Encode(&buf)

	data := buf.Bytes()
	readBuf := make([]byte, MaxPayloadSize+HeaderSize)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decoded := &Frame{}
		decoded.Decode(bytes.NewReader(data), readBuf)
	}
}
