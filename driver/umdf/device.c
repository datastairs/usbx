// device.c — USB device emulation and control device for IOCTL communication.
//
// Creates TWO WDF devices:
//   1. USB Device (UsbxUsbDevice) — presents as a real USB device to Windows.
//      The hypervisor can pass this through to a VM.
//   2. Control Device (UsbxControlDevice) — exposes IOCTLs for the Go program
//      on the same machine to read/write endpoint data.

#include "driver.h"
#include "device.h"
#include "trace.h"

// Per-endpoint ring buffer with read/write pointers.
typedef struct _USBX_ENDPOINT_BUFFER {
    UCHAR*  Data;
    ULONG   Size;
    volatile LONG ReadPos;
    volatile LONG WritePos;
    volatile LONG Available;  // Bytes available to read
} USBX_ENDPOINT_BUFFER;

// Device context for the USB device emulation.
typedef struct _USBX_DEVICE_CONTEXT {
    WDFUSBDEVICE    UsbDevice;
    WDFDEVICE       ControlDevice;

    // Endpoint ring buffers
    USBX_ENDPOINT_BUFFER Ep1In;   // B→A: B (USB host) writes via USB, Go reads via IOCTL
    USBX_ENDPOINT_BUFFER Ep2Out;  // A→B: Go writes via IOCTL, B (USB host) reads via USB

    // Control device queue
    WDFQUEUE        IoQueue;
} USBX_DEVICE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(USBX_DEVICE_CONTEXT, UsbxGetDeviceContext)

// Forward declarations
static NTSTATUS UsbxCreateUsbDevice(_In_ WDFDEVICE Device);
static NTSTATUS UsbxCreateControlDevice(_In_ WDFDEVICE Device);
static VOID UsbxIoDeviceControl(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request,
                                 _In_ size_t OutputBufferLength, _In_ size_t InputBufferLength,
                                 _In_ ULONG IoControlCode);
static VOID UsbxUsbIoReadEp1(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request, _In_ size_t Length);
static VOID UsbxUsbIoWriteEp2(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request, _In_ size_t Length);

// Initialize an endpoint ring buffer.
static VOID UsbxInitEndpoint(_In_ USBX_ENDPOINT_BUFFER* Ep)
{
    Ep->Data = (UCHAR*)ExAllocatePool2(POOL_FLAG_NON_PAGED, USBX_EP_BUFFER_SIZE, 'XUBX');
    if (Ep->Data) {
        RtlZeroMemory(Ep->Data, USBX_EP_BUFFER_SIZE);
    }
    Ep->Size = USBX_EP_BUFFER_SIZE;
    Ep->ReadPos = 0;
    Ep->WritePos = 0;
    Ep->Available = 0;
}

// Write data to endpoint buffer (returns bytes written, 0 if full).
static ULONG UsbxEpWrite(_In_ USBX_ENDPOINT_BUFFER* Ep, _In_ const UCHAR* Data, _In_ ULONG Len)
{
    if (!Ep->Data || Len == 0) return 0;

    ULONG space = Ep->Size - Ep->Available;
    if (Len > space) Len = space;
    if (Len == 0) return 0;

    ULONG wp = Ep->WritePos;
    ULONG first = Ep->Size - wp;
    if (Len <= first) {
        RtlCopyMemory(Ep->Data + wp, Data, Len);
    } else {
        RtlCopyMemory(Ep->Data + wp, Data, first);
        RtlCopyMemory(Ep->Data, Data + first, Len - first);
    }

    Ep->WritePos = (wp + Len) % Ep->Size;
    InterlockedAdd(&Ep->Available, (LONG)Len);
    return Len;
}

// Read data from endpoint buffer (returns bytes read, 0 if empty).
static ULONG UsbxEpRead(_In_ USBX_ENDPOINT_BUFFER* Ep, _Out_ UCHAR* Buf, _In_ ULONG MaxLen)
{
    if (!Ep->Data || Ep->Available == 0) return 0;

    ULONG avail = (ULONG)InterlockedCompareExchange(&Ep->Available, 0, 0);
    if (avail == 0) return 0;
    if (MaxLen > avail) MaxLen = avail;

    ULONG rp = Ep->ReadPos;
    ULONG first = Ep->Size - rp;
    if (MaxLen <= first) {
        RtlCopyMemory(Buf, Ep->Data + rp, MaxLen);
    } else {
        RtlCopyMemory(Buf, Ep->Data + rp, first);
        RtlCopyMemory(Buf + first, Ep->Data, MaxLen - first);
    }

    Ep->ReadPos = (rp + MaxLen) % Ep->Size;
    InterlockedAdd(&Ep->Available, -(LONG)MaxLen);
    return MaxLen;
}

// Standard USB device descriptor (18 bytes).
static const UCHAR UsbxDeviceDescriptor[] = {
    0x12,       // bLength (18)
    0x01,       // bDescriptorType (DEVICE)
    0x00, 0x02, // bcdUSB (2.0)
    0xFF,       // bDeviceClass (vendor-specific)
    0x00,       // bDeviceSubClass
    0x00,       // bDeviceProtocol
    0x40,       // bMaxPacketSize0 (64)
    0x53, 0x55, // idVendor (0x5553)
    0x58, 0x42, // idProduct (0x4258)
    0x00, 0x01, // bcdDevice (1.00)
    0x00,       // iManufacturer
    0x00,       // iProduct
    0x00,       // iSerialNumber
    0x01,       // bNumConfigurations
};

// Configuration descriptor (config + interface + 2 endpoints = 32 bytes).
static const UCHAR UsbxConfigDescriptor[] = {
    // Configuration descriptor (9 bytes)
    0x09,       // bLength
    0x02,       // bDescriptorType (CONFIGURATION)
    0x20, 0x00, // wTotalLength (32)
    0x01,       // bNumInterfaces
    0x01,       // bConfigurationValue
    0x00,       // iConfiguration
    0x80,       // bmAttributes (bus-powered)
    0x32,       // bMaxPower (100 mA)

    // Interface descriptor (9 bytes)
    0x09,       // bLength
    0x04,       // bDescriptorType (INTERFACE)
    0x00,       // bInterfaceNumber
    0x00,       // bAlternateSetting
    0x02,       // bNumEndpoints
    0xFF,       // bInterfaceClass (vendor-specific)
    0x00,       // bInterfaceSubClass
    0x00,       // bInterfaceProtocol
    0x00,       // iInterface

    // Endpoint 1: Bulk IN (7 bytes) — B→A
    0x07,       // bLength
    0x05,       // bDescriptorType (ENDPOINT)
    0x81,       // bEndpointAddress (IN, ep 1)
    0x02,       // bmAttributes (Bulk)
    0x00, 0x02, // wMaxPacketSize (512)
    0x00,       // bInterval

    // Endpoint 2: Bulk OUT (7 bytes) — A→B
    0x07,       // bLength
    0x05,       // bDescriptorType (ENDPOINT)
    0x02,       // bEndpointAddress (OUT, ep 2)
    0x02,       // bmAttributes (Bulk)
    0x00, 0x02, // wMaxPacketSize (512)
    0x00,       // bInterval
};

// ─── Public API ────────────────────────────────────────────────────────────

NTSTATUS UsbxCreateDevice(_In_ WDFDRIVER Driver, _Inout_ PWDFDEVICE_INIT DeviceInit)
{
    NTSTATUS status;
    WDFDEVICE device;
    USBX_DEVICE_CONTEXT* ctx;

    // Create the USB device emulation.
    status = WdfDeviceCreate(&DeviceInit, WDF_NO_OBJECT_ATTRIBUTES, &device);
    if (!NT_SUCCESS(status)) {
        TraceError("WdfDeviceCreate failed: %!STATUS!", status);
        return status;
    }

    ctx = UsbxGetDeviceContext(device);
    RtlZeroMemory(ctx, sizeof(USBX_DEVICE_CONTEXT));

    // Init endpoint ring buffers (1MB each)
    UsbxInitEndpoint(&ctx->Ep1In);
    UsbxInitEndpoint(&ctx->Ep2Out);

    // Create the USB device emulation
    status = UsbxCreateUsbDevice(device);
    if (!NT_SUCCESS(status)) {
        TraceError("UsbxCreateUsbDevice failed: %!STATUS!", status);
        return status;
    }

    // Create the control device for Go IOCTL communication
    status = UsbxCreateControlDevice(device);
    if (!NT_SUCCESS(status)) {
        TraceError("UsbxCreateControlDevice failed: %!STATUS!", status);
        return status;
    }

    TraceInfo("USBX device created successfully");
    return STATUS_SUCCESS;
}

// ─── USB Device Emulation ──────────────────────────────────────────────────

static NTSTATUS UsbxCreateUsbDevice(_In_ WDFDEVICE Device)
{
    NTSTATUS status;
    USBX_DEVICE_CONTEXT* ctx = UsbxGetDeviceContext(Device);
    WDF_OBJECT_ATTRIBUTES attributes;
    WDF_IO_QUEUE_CONFIG queueConfig;
    WDFQUEUE queue;

    // Set USB device descriptor
    WDF_USB_DEVICE_CREATE_CONFIG usbConfig;
    WDF_USB_DEVICE_CREATE_CONFIG_INIT(&usbConfig, UsbxDeviceDescriptor, sizeof(UsbxDeviceDescriptor));

    status = WdfUsbTargetDeviceCreate(Device, WDF_NO_OBJECT_ATTRIBUTES, &ctx->UsbDevice);
    if (!NT_SUCCESS(status)) return status;

    // Set configuration descriptor
    WDF_USB_DEVICE_SELECT_CONFIG_PARAMS configParams;
    WDF_USB_DEVICE_SELECT_CONFIG_PARAMS_INIT_MULTIPLE_INTERFACES(&configParams, 0, NULL);
    configParams.Type = WdfUsbTargetDeviceSelectConfigTypeDescriptor;
    configParams.ConfigDescriptor.Descriptor = (PUCHAR)UsbxConfigDescriptor;
    configParams.ConfigDescriptor.Length = sizeof(UsbxConfigDescriptor);

    status = WdfUsbTargetDeviceSelectConfig(ctx->UsbDevice, WDF_NO_OBJECT_ATTRIBUTES, &configParams);
    if (!NT_SUCCESS(status)) return status;

    // Create I/O queues for each endpoint

    // EP1 IN (B→A): USB host reads from this endpoint
    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueSequential);
    queueConfig.EvtIoRead = UsbxUsbIoReadEp1;
    status = WdfIoQueueCreate(Device, &queueConfig, WDF_NO_OBJECT_ATTRIBUTES, &queue);
    if (!NT_SUCCESS(status)) return status;

    // EP2 OUT (A→B): USB host writes to this endpoint
    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueSequential);
    queueConfig.EvtIoWrite = UsbxUsbIoWriteEp2;
    status = WdfIoQueueCreate(Device, &queueConfig, WDF_NO_OBJECT_ATTRIBUTES, &queue);
    if (!NT_SUCCESS(status)) return status;

    TraceInfo("USB device emulation configured: VID=0x%04X PID=0x%04X", USBX_VID, USBX_PID);
    return STATUS_SUCCESS;
}

// EP1 IN read handler: USB host (B via VM passthrough) reads from EP1 IN.
// We provide data from the Ep1In ring buffer (data written by Go via IOCTL_WRITE_OUT? No...)
// Wait — EP1 IN is B→A direction. B writes via USB, we store in Ep1In. Go reads via IOCTL_READ_IN.
// But USB IN read means B READS from us. Where does B read from?
//
// Correct model:
//   EP1 (0x81) Bulk IN:  Device→Host.  B (USB host) READS IN endpoint → gets data from Ep2Out buffer.
//   EP2 (0x02) Bulk OUT: Host→Device.  B (USB host) WRITES OUT endpoint → we store in Ep1In buffer.
//
// So:
//   Go writes data → stored in Ep2Out → B reads via EP1 IN read
//   B writes data → stored in Ep1In → Go reads via IOCTL_READ_IN
//
// In USB terms:
//   EP1 IN = device provides data to host = Go's data flowing to B
//   EP2 OUT = host provides data to device = B's data flowing to Go
//
// But this is confusing. Let me re-label:
//   EP1 (0x81) Bulk IN:  Device provides data. Go writes here → B reads.
//   EP2 (0x02) Bulk OUT: Host provides data. B writes here → Go reads.

static VOID UsbxUsbIoReadEp1(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request, _In_ size_t Length)
{
    // Host (B) is reading from EP1 IN — provide data from Go's writes.
    // Actually: EP1 IN read → we return data from Ep2Out ring buffer.
    WDFDEVICE device = WdfIoQueueGetDevice(Queue);
    USBX_DEVICE_CONTEXT* ctx = UsbxGetDeviceContext(device);

    NTSTATUS status = STATUS_SUCCESS;
    ULONG bytesRead = 0;

    if (Length > 0) {
        WDFMEMORY memory;
        status = WdfRequestRetrieveOutputMemory(Request, &memory);
        if (NT_SUCCESS(status)) {
            PVOID buffer;
            size_t bufSize;
            buffer = WdfMemoryGetBuffer(memory, &bufSize);
            if (bufSize > Length) bufSize = Length;
            bytesRead = UsbxEpRead(&ctx->Ep2Out, (UCHAR*)buffer, (ULONG)bufSize);
        }
    }

    WdfRequestCompleteWithInformation(Request, status, bytesRead);
}

// EP2 OUT write handler: USB host (B via VM passthrough) writes to EP2 OUT.
// Data goes to Ep1In ring buffer for Go to read via IOCTL_READ_IN.
static VOID UsbxUsbIoWriteEp2(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request, _In_ size_t Length)
{
    WDFDEVICE device = WdfIoQueueGetDevice(Queue);
    USBX_DEVICE_CONTEXT* ctx = UsbxGetDeviceContext(device);

    NTSTATUS status = STATUS_SUCCESS;
    ULONG bytesWritten = 0;

    if (Length > 0) {
        WDFMEMORY memory;
        status = WdfRequestRetrieveInputMemory(Request, &memory);
        if (NT_SUCCESS(status)) {
            PVOID buffer;
            size_t bufSize;
            buffer = WdfMemoryGetBuffer(memory, &bufSize);
            if (bufSize > Length) bufSize = Length;
            bytesWritten = UsbxEpWrite(&ctx->Ep1In, (UCHAR*)buffer, (ULONG)bufSize);
        }
    }

    WdfRequestCompleteWithInformation(Request, status, bytesWritten);
}

// ─── Control Device (Go IOCTL interface) ───────────────────────────────────

static NTSTATUS UsbxCreateControlDevice(_In_ WDFDEVICE Device)
{
    NTSTATUS status;
    USBX_DEVICE_CONTEXT* ctx = UsbxGetDeviceContext(Device);
    PWDFDEVICE_INIT pInit = NULL;
    WDFDEVICE controlDevice;
    WDF_IO_QUEUE_CONFIG queueConfig;
    WDFQUEUE queue;
    DECLARE_CONST_UNICODE_STRING(ntDeviceName, L"\\Device\\USBXControl");
    DECLARE_CONST_UNICODE_STRING(symlinkName, L"\\DosDevices\\USBXControl");

    // Allocate a new WDFDEVICE_INIT for the control device
    pInit = WdfControlDeviceInitAllocate(WdfDeviceGetDriver(Device),
                                          &SDDL_DEVOBJ_SYS_ALL_ADM_ALL);
    if (!pInit) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }

    WdfDeviceInitSetDeviceType(pInit, FILE_DEVICE_UNKNOWN);
    WdfDeviceInitSetCharacteristics(pInit, FILE_DEVICE_SECURE_OPEN, FALSE);
    WdfDeviceInitSetDeviceClass(pInit, &GUID_DEVCLASS_SYSTEM);
    WdfDeviceInitAssignName(pInit, &ntDeviceName);

    status = WdfDeviceCreate(&pInit, WDF_NO_OBJECT_ATTRIBUTES, &controlDevice);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    // Create symbolic link so Go can open \\.\USBXControl
    status = WdfDeviceCreateSymbolicLink(controlDevice, &symlinkName);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    ctx->ControlDevice = controlDevice;

    // Create IOCTL queue
    WDF_IO_QUEUE_CONFIG_INIT_DEFAULT_QUEUE(&queueConfig, WdfIoQueueSequential);
    queueConfig.EvtIoDeviceControl = UsbxIoDeviceControl;

    status = WdfIoQueueCreate(controlDevice, &queueConfig, WDF_NO_OBJECT_ATTRIBUTES, &queue);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    ctx->IoQueue = queue;

    TraceInfo("Control device created: \\\\.\\USBXControl");
    return STATUS_SUCCESS;
}

static VOID UsbxIoDeviceControl(
    _In_ WDFQUEUE   Queue,
    _In_ WDFREQUEST Request,
    _In_ size_t     OutputBufferLength,
    _In_ size_t     InputBufferLength,
    _In_ ULONG      IoControlCode
)
{
    WDFDEVICE controlDevice = WdfIoQueueGetDevice(Queue);
    WDFDEVICE parentDevice = WdfControlDeviceGetParentDevice(controlDevice);
    USBX_DEVICE_CONTEXT* ctx = UsbxGetDeviceContext(parentDevice);

    NTSTATUS status = STATUS_INVALID_DEVICE_REQUEST;
    ULONG_PTR info = 0;

    switch (IoControlCode) {
    case IOCTL_USBX_WRITE_OUT:
        // Go writes data to be sent to B. Stored in Ep2Out ring buffer.
        // B (USB host via VM passthrough) reads this via EP1 IN.
        if (InputBufferLength > 0) {
            WDFMEMORY memory;
            status = WdfRequestRetrieveInputMemory(Request, &memory);
            if (NT_SUCCESS(status)) {
                PVOID buffer;
                size_t bufSize;
                buffer = WdfMemoryGetBuffer(memory, &bufSize);
                if (bufSize > InputBufferLength) bufSize = InputBufferLength;
                info = UsbxEpWrite(&ctx->Ep2Out, (UCHAR*)buffer, (ULONG)bufSize);
                status = (info > 0) ? STATUS_SUCCESS : STATUS_BUFFER_OVERFLOW;
            }
        } else {
            status = STATUS_INVALID_PARAMETER;
        }
        break;

    case IOCTL_USBX_READ_IN:
        // Go reads data sent by B. Retrieved from Ep1In ring buffer.
        // B (USB host via VM passthrough) writes this via EP2 OUT.
        if (OutputBufferLength > 0) {
            WDFMEMORY memory;
            status = WdfRequestRetrieveOutputMemory(Request, &memory);
            if (NT_SUCCESS(status)) {
                PVOID buffer;
                size_t bufSize;
                buffer = WdfMemoryGetBuffer(memory, &bufSize);
                if (bufSize > OutputBufferLength) bufSize = OutputBufferLength;
                info = UsbxEpRead(&ctx->Ep1In, (UCHAR*)buffer, (ULONG)bufSize);
                status = STATUS_SUCCESS; // 0 bytes is OK (no data available)
            }
        } else {
            status = STATUS_INVALID_PARAMETER;
        }
        break;

    case IOCTL_USBX_GET_STATE:
        // Return available bytes in each endpoint buffer
        if (OutputBufferLength >= 8) {
            WDFMEMORY memory;
            status = WdfRequestRetrieveOutputMemory(Request, &memory);
            if (NT_SUCCESS(status)) {
                ULONG state[2];
                state[0] = (ULONG)InterlockedCompareExchange(&ctx->Ep2Out.Available, 0, 0);
                state[1] = (ULONG)InterlockedCompareExchange(&ctx->Ep1In.Available, 0, 0);
                RtlCopyMemory(WdfMemoryGetBuffer(memory, NULL), state, sizeof(state));
                info = sizeof(state);
                status = STATUS_SUCCESS;
            }
        }
        break;

    case IOCTL_USBX_CLEAR_EP:
        ctx->Ep1In.ReadPos = ctx->Ep1In.WritePos;
        ctx->Ep1In.Available = 0;
        ctx->Ep2Out.ReadPos = ctx->Ep2Out.WritePos;
        ctx->Ep2Out.Available = 0;
        status = STATUS_SUCCESS;
        break;

    default:
        status = STATUS_INVALID_DEVICE_REQUEST;
        break;
    }

    WdfRequestCompleteWithInformation(Request, status, info);
}
