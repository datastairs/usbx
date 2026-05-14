// device.h — Device creation API

#pragma once

#include <wdf.h>

NTSTATUS UsbxCreateDevice(_In_ WDFDRIVER Driver, _Inout_ PWDFDEVICE_INIT DeviceInit);
