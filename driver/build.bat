@echo off
REM Build script for USBX UMDF2 Virtual USB Driver
REM
REM Prerequisites:
REM   1. Visual Studio 2022 (Community edition is free)
REM   2. Windows Driver Kit (WDK) for Windows 11 / Server 2022
REM      Download: https://learn.microsoft.com/en-us/windows-hardware/drivers/download-the-wdk
REM   3. Open "x64 Native Tools Command Prompt for VS 2022" (as Administrator)
REM
REM Then run: build.bat

setlocal

set DRIVER_NAME=usbx
set SRC_DIR=%~dp0umdf

echo ============================================
echo  USBX UMDF2 Driver Build
echo ============================================

REM Compile driver source files
cl.exe /nologo /W4 /WX /O2 /GS /Gy ^
       /D_UNICODE /DUNICODE ^
       /I%WDKCONTENTROOT%\Include\km\1.11 ^
       /I%WDKCONTENTROOT%\Include\umdf\2.0 ^
       /I%WDKCONTENTROOT%\Include\shared ^
       /Fo%SRC_DIR%\ ^
       /c ^
       %SRC_DIR%\driver.c ^
       %SRC_DIR%\device.c

if %ERRORLEVEL% neq 0 (
    echo ERROR: Compilation failed
    exit /b 1
)

REM Link into UMDF driver DLL
link.exe /NOLOGO /DLL /OUT:%DRIVER_NAME%.dll ^
         /SUBSYSTEM:CONSOLE ^
         /MACHINE:X64 ^
         /ENTRY:FxDriverEntry ^
         %WDKCONTENTROOT%\Lib\km\1.11\x64\WdfDriverEntry.lib ^
         %WDKCONTENTROOT%\Lib\umdf\2.0\x64\WdfLdr.lib ^
         %SRC_DIR%\driver.obj ^
         %SRC_DIR%\device.obj ^
         onecoreuap.lib

if %ERRORLEVEL% neq 0 (
    echo ERROR: Link failed
    exit /b 1
)

REM Generate INF from INX template
copy /Y %SRC_DIR%\usbx.inx usbx.inf >nul

echo.
echo ============================================
echo  Build complete: %DRIVER_NAME%.dll
echo ============================================
echo.
echo Installation steps:
echo   1. Enable test signing (as Admin):
echo      bcdedit /set testsigning on
echo      (reboot required)
echo.
echo   2. Copy usbx.dll and usbx.inf to a directory
echo.
echo   3. Install driver:
echo      pnputil /add-driver usbx.inf /install
echo.
echo   4. Verify:
echo      pnputil /enum-drivers ^| findstr USBX
echo.
echo   5. The control device will appear as \\.\USBXControl
echo   6. The virtual USB device will appear in Device Manager
echo      (VID=0x5553, PID=0x4258) and can be passed through to a VM.
echo.
echo   7. Run side-a:
echo      side-a -transport ioctl -socks5 :1080

endlocal
