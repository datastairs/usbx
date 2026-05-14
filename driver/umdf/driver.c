// USBX Virtual USB Device — UMDF2 User-Mode Driver
//
// Creates a virtual USB device (VID=0x5553, PID=0x4258) with two bulk endpoints.
// The device appears as a real USB device to Windows and can be passed through
// to a VM via Hypervisor USB passthrough.
//
// Architecture:
//
//   Go Program (side-a)              UMDF Driver            USB Host (side-b, VM)
//       │                              │                         │
//       │── IOCTL_WRITE_OUT ────────→ EP2 OUT buffer ──→ USB read EP2 OUT ──│
//       │                              │                         │
//       │←── IOCTL_READ_IN ───────── EP1 IN buffer ←── USB write EP1 IN ───│
//       │                              │                         │
//
// The driver presents a second "control" device node for the Go program
// to communicate via IOCTL. The USB endpoints are internal ring buffers.

#include <windows.h>
#include <wdf.h>
#include <usb.h>
#include <wdfusb.h>

#include "driver.h"
#include "device.h"
#include "trace.h"

// Driver entry points
DRIVER_INITIALIZE DriverEntry;
EVT_WDF_DRIVER_DEVICE_ADD UsbxEvtDeviceAdd;
EVT_WDF_DRIVER_UNLOAD UsbxEvtDriverUnload;

NTSTATUS
DriverEntry(
    _In_ PDRIVER_OBJECT  DriverObject,
    _In_ PUNICODE_STRING RegistryPath
)
{
    WDF_DRIVER_CONFIG config;
    WDF_DRIVER_CONFIG_INIT(&config, UsbxEvtDeviceAdd);
    config.EvtDriverUnload = UsbxEvtDriverUnload;

    return WdfDriverCreate(DriverObject, RegistryPath,
                          WDF_NO_OBJECT_ATTRIBUTES, &config, WDF_NO_HANDLE);
}

NTSTATUS
UsbxEvtDeviceAdd(
    _In_    WDFDRIVER       Driver,
    _Inout_ PWDFDEVICE_INIT DeviceInit
)
{
    // Create the USB device emulation and control device.
    return UsbxCreateDevice(Driver, DeviceInit);
}

VOID
UsbxEvtDriverUnload(_In_ WDFDRIVER Driver)
{
    UNREFERENCED_PARAMETER(Driver);
    TraceInfo("USBX driver unloaded");
}
