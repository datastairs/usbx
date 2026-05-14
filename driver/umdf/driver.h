// driver.h — Common definitions for USBX UMDF driver

#pragma once

#include <windows.h>
#include <wdf.h>

// USB descriptor constants
#define USBX_VID         0x5553
#define USBX_PID         0x4258
#define USBX_BCD_DEVICE  0x0100

// Endpoint addresses (USB view: IN = device→host, OUT = host→device)
#define USBX_EP1_BULK_IN   0x81  // B→A direction (Go reads via IOCTL_READ_IN)
#define USBX_EP2_BULK_OUT  0x02  // A→B direction (Go writes via IOCTL_WRITE_OUT)

// Endpoint ring buffer sizes
#define USBX_EP_BUFFER_SIZE  0x100000  // 1 MB per endpoint

// IOCTL codes for control device (Go ↔ Driver communication)
#define USBX_IOCTL_TYPE  0x8000

// Write data to EP2 OUT buffer (A→B). Go calls this to send proxy requests.
#define IOCTL_USBX_WRITE_OUT   CTL_CODE(USBX_IOCTL_TYPE, 0x901, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)

// Read data from EP1 IN buffer (B→A). Go calls this to get internet responses.
#define IOCTL_USBX_READ_IN     CTL_CODE(USBX_IOCTL_TYPE, 0x902, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)

// Clear endpoint buffers (on reset).
#define IOCTL_USBX_CLEAR_EP    CTL_CODE(USBX_IOCTL_TYPE, 0x903, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)

// Get endpoint read/write positions (for polling).
#define IOCTL_USBX_GET_STATE   CTL_CODE(USBX_IOCTL_TYPE, 0x904, METHOD_BUFFERED, FILE_READ_DATA)

// Control device name that Go opens via CreateFile
#define USBX_CONTROL_DEVICE_NAME  L"\\DosDevices\\USBXControl"
#define USBX_CONTROL_SYMLINK      L"\\\\.\\USBXControl"

// USB device hardware ID for INF matching
#define USBX_HARDWARE_ID  L"USB\\VID_5553&PID_4258&REV_0100"
