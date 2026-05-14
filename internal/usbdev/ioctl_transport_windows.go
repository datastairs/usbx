//go:build windows

package usbdev

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"usbx/internal/protocol"
)

const (
	ioctlWriteOut    = 0x80002000 | (0x901 << 2)
	ioctlReadIn      = 0x80002000 | (0x902 << 2)
	ioctlGetState    = 0x80002000 | (0x904 << 2)
	ioctlClearEp     = 0x80002000 | (0x903 << 2)

	maxTransferSize = 65536
	pollInterval    = 2 * time.Millisecond
)

var (
	modKernel32           = syscall.NewLazyDLL("kernel32.dll")
	procDeviceIoControl   = modKernel32.NewProc("DeviceIoControl")
	procCreateFileW       = modKernel32.NewProc("CreateFileW")
)

type ioctlTransport struct {
	handle syscall.Handle

	state     atomic.Int32
	closeOnce sync.Once
	closeCh   chan struct{}

	readBuf []byte
	readMu  sync.Mutex
	readPos int
	readEnd int

	writeMu sync.Mutex

	rxBytes  atomic.Uint64
	txBytes  atomic.Uint64
	rxFrames atomic.Uint64
	txFrames atomic.Uint64
}

func NewIOCTLTransport() (Transport, error) {
	name, err := syscall.UTF16PtrFromString(`\\.\USBXControl`)
	if err != nil {
		return nil, fmt.Errorf("usbdev: encode device name: %w", err)
	}

	r1, _, e1 := procCreateFileW.Call(
		uintptr(unsafe.Pointer(name)),
		uintptr(syscall.GENERIC_READ|syscall.GENERIC_WRITE),
		0, 0,
		uintptr(syscall.OPEN_EXISTING),
		uintptr(syscall.FILE_FLAG_OVERLAPPED),
		0,
	)
	handle := syscall.Handle(r1)
	if handle == syscall.InvalidHandle || handle == 0 {
		return nil, fmt.Errorf("usbdev: open \\\\.\\USBXControl: %w", e1)
	}

	t := &ioctlTransport{
		handle:  handle,
		closeCh: make(chan struct{}),
		readBuf: make([]byte, maxTransferSize),
	}

	t.state.Store(int32(StateAttached))
	log.Print("[usbdev] IOCTL transport connected to USBX driver")
	return t, nil
}

func (t *ioctlTransport) State() USBState    { return USBState(t.state.Load()) }
func (t *ioctlTransport) SetState(s USBState) { t.state.Store(int32(s)) }
func (t *ioctlTransport) IsConfigured() bool  { return t.State() == StateConfigured }

func (t *ioctlTransport) WriteFrame(f *protocol.Frame) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	totalLen := protocol.HeaderSize + len(f.Payload)
	buf := protocol.AcquireBuffer()
	defer protocol.ReleaseBuffer(buf)

	n := protocol.EncodeInto(buf[:totalLen], f)
	bytesRet, err := t.ioctl(ioctlWriteOut, buf[:n], nil)
	if err != nil {
		return fmt.Errorf("usbdev: IOCTL_WRITE_OUT: %w", err)
	}

	t.txBytes.Add(uint64(bytesRet))
	t.txFrames.Add(1)
	return nil
}

func (t *ioctlTransport) ReadFrame() (*protocol.Frame, error) {
	for {
		t.readMu.Lock()
		if t.readPos < t.readEnd {
			f, n, ok := t.tryDecode()
			t.readMu.Unlock()
			if ok {
				t.rxBytes.Add(uint64(n))
				t.rxFrames.Add(1)
				return f, nil
			}
		} else {
			t.readPos = 0
			t.readEnd = 0
			t.readMu.Unlock()
		}

		buf := t.readBuf
		n, err := t.ioctl(ioctlReadIn, nil, buf)
		if err != nil {
			select {
			case <-t.closeCh:
				return nil, fmt.Errorf("usbdev: transport closed")
			case <-time.After(pollInterval):
			}
			continue
		}

		if n == 0 {
			select {
			case <-t.closeCh:
				return nil, fmt.Errorf("usbdev: transport closed")
			case <-time.After(pollInterval):
			}
			continue
		}

		t.readMu.Lock()
		t.readPos = 0
		t.readEnd = n
		f, total, ok := t.tryDecode()
		t.readMu.Unlock()
		if ok {
			t.rxBytes.Add(uint64(total))
			t.rxFrames.Add(1)
			return f, nil
		}
	}
}

func (t *ioctlTransport) tryDecode() (*protocol.Frame, int, bool) {
	data := t.readBuf[t.readPos:t.readEnd]
	if len(data) < protocol.HeaderSize {
		return nil, 0, false
	}

	payloadLen := int(data[12])<<24 | int(data[13])<<16 | int(data[14])<<8 | int(data[15])
	total := protocol.HeaderSize + payloadLen
	if len(data) < total {
		return nil, 0, false
	}

	f := protocol.AcquireFrame()
	if err := protocol.DecodeFrom(f, data[:total]); err != nil {
		protocol.ReleaseFrame(f)
		t.readPos = t.readEnd
		return nil, 0, false
	}

	if len(f.Payload) > 0 {
		cp := make([]byte, len(f.Payload))
		copy(cp, f.Payload)
		f.Payload = cp
	}

	t.readPos += total
	return f, total, true
}

func (t *ioctlTransport) ioctl(code uint32, in, out []byte) (int, error) {
	var inPtr, outPtr uintptr
	var inLen, outLen uint32
	if in != nil {
		inPtr = uintptr(unsafe.Pointer(&in[0]))
		inLen = uint32(len(in))
	}
	if out != nil {
		outPtr = uintptr(unsafe.Pointer(&out[0]))
		outLen = uint32(len(out))
	}

	var bytesRet uint32
	r1, _, e1 := procDeviceIoControl.Call(
		uintptr(t.handle), uintptr(code),
		inPtr, uintptr(inLen),
		outPtr, uintptr(outLen),
		uintptr(unsafe.Pointer(&bytesRet)),
		0,
	)
	if r1 == 0 {
		return 0, fmt.Errorf("DeviceIoControl(0x%x): %w", code, e1)
	}
	return int(bytesRet), nil
}

func (t *ioctlTransport) Stats() (uint64, uint64, uint64, uint64) {
	return t.rxBytes.Load(), t.txBytes.Load(), t.rxFrames.Load(), t.txFrames.Load()
}

func (t *ioctlTransport) Close() error {
	t.closeOnce.Do(func() {
		t.state.Store(int32(StateDetached))
		close(t.closeCh)
	})
	return syscall.CloseHandle(t.handle)
}

type ioctlAddr struct{}

func (a ioctlAddr) Network() string { return "ioctl" }
func (a ioctlAddr) String() string  { return `\\.\USBXControl` }

func (t *ioctlTransport) LocalAddr() net.Addr { return ioctlAddr{} }
