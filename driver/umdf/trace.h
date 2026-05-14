// trace.h — Minimal tracing macros for UMDF driver (no WPP dependency)

#pragma once

#include <windows.h>

#define TraceInfo(fmt, ...)   DbgPrintEx(DPFLTR_IHVDRIVER_ID, DPFLTR_INFO_LEVEL,    "[USBX] " fmt "\n", ##__VA_ARGS__)
#define TraceError(fmt, ...)  DbgPrintEx(DPFLTR_IHVDRIVER_ID, DPFLTR_ERROR_LEVEL,   "[USBX] " fmt "\n", ##__VA_ARGS__)
