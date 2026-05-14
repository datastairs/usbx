#!/bin/bash
# setup-gadget.sh — Configure a Linux USB Gadget via ConfigFS + FunctionFS.
#
# Creates a virtual USB device (VID=0x5553 PID=0x4258) with two bulk endpoints
# that can be passed through to a VM via the hypervisor.
#
# The gadget acts as a communication channel:
#   side-a (Linux host, gadget) <--> VM B (USB host, running side-b -transport usb)
#
# Usage:
#   sudo ./scripts/setup-gadget.sh              # auto-detect UDC
#   sudo ./scripts/setup-gadget.sh <udc-name>   # specify UDC (e.g. dummy_hcd.0)
#   sudo ./scripts/setup-gadget.sh --cleanup    # remove the gadget
#
# Prerequisites:
#   - Linux kernel with CONFIG_USB_GADGET=y and CONFIG_USB_FUNCTIONFS=y
#   - For virtual UDC (no hardware): modprobe dummy_hcd
#   - root privileges

set -euo pipefail

GADGET_NAME="usbx"
CONFIGFS="/sys/kernel/config/usb_gadget"
GADGET_DIR="${CONFIGFS}/${GADGET_NAME}"
FFS_DIR="/dev/usb-ffs/${GADGET_NAME}"
FFS_NAME="ffs.usb0"

VID="0x5553"
PID="0x4258"
BCD_DEVICE="0x0100"
BCD_USB="0x0200"

DEVICE_CLASS="0xFF"
DEVICE_SUB_CLASS="0x00"
DEVICE_PROTOCOL="0x00"

CONFIG_NAME="c.1"
CONFIG_LABEL="USBX Config"
MAX_POWER="100"

cleanup() {
    echo "[*] Cleaning up existing gadget '${GADGET_NAME}'..."

    # Unbind UDC if bound.
    if [ -f "${GADGET_DIR}/UDC" ]; then
        echo "" > "${GADGET_DIR}/UDC" 2>/dev/null || true
    fi

    # Remove symlinks from config.
    if [ -d "${GADGET_DIR}/configs/${CONFIG_NAME}" ]; then
        for link in "${GADGET_DIR}/configs/${CONFIG_NAME}/"*; do
            [ -L "$link" ] && rm -f "$link" 2>/dev/null || true
        done
        rmdir "${GADGET_DIR}/configs/${CONFIG_NAME}" 2>/dev/null || true
    fi

    # Remove function.
    if [ -d "${GADGET_DIR}/functions/${FFS_NAME}" ]; then
        rmdir "${GADGET_DIR}/functions/${FFS_NAME}" 2>/dev/null || true
    fi

    # Remove gadget directory.
    if [ -d "${GADGET_DIR}" ]; then
        rmdir "${GADGET_DIR}" 2>/dev/null || true
    fi

    # Unmount FunctionFS if mounted.
    if mountpoint -q "${FFS_DIR}" 2>/dev/null; then
        umount "${FFS_DIR}" 2>/dev/null || true
    fi

    echo "[+] Cleanup complete."
}

# --cleanup flag.
if [ "${1:-}" = "--cleanup" ]; then
    cleanup
    exit 0
fi

# Ensure running as root.
if [ "$(id -u)" -ne 0 ]; then
    echo "[-] This script must be run as root (sudo)." >&2
    exit 1
fi

# Load required kernel modules.
echo "[*] Loading kernel modules..."
modprobe libcomposite 2>/dev/null || true
modprobe usb_f_fs 2>/dev/null || true

# Determine UDC (USB Device Controller).
if [ $# -ge 1 ]; then
    UDC="$1"
else
    # Auto-detect: prefer dummy_hcd if available (virtual, no hardware needed),
    # otherwise use the first available UDC.
    if [ -d /sys/class/udc ]; then
        UDC_LIST=($(ls /sys/class/udc/))
        if [ ${#UDC_LIST[@]} -eq 0 ]; then
            echo "[-] No UDC found. Load dummy_hcd for a virtual UDC:" >&2
            echo "    sudo modprobe dummy_hcd" >&2
            exit 1
        fi
        # Prefer dummy_hcd for virtual operation.
        for udc in "${UDC_LIST[@]}"; do
            if [[ "$udc" == dummy_hcd* ]]; then
                UDC="$udc"
                break
            fi
        done
        if [ -z "${UDC:-}" ]; then
            UDC="${UDC_LIST[0]}"
            echo "[!] No dummy_hcd found, using hardware UDC: ${UDC}"
            echo "[!] (For virtual USB without hardware, run: sudo modprobe dummy_hcd)"
        fi
    else
        echo "[-] /sys/class/udc not found. Is USB gadget support enabled?" >&2
        echo "    Check: CONFIG_USB_GADGET=y in kernel config." >&2
        exit 1
    fi
fi

echo "[*] Using UDC: ${UDC}"

# Clean up any previous instance.
cleanup

# Create directories. Use subshells with || true to avoid set -e issues
# when rmdir fails because the directory doesn't exist.

echo "[*] Creating ConfigFS gadget..."

# 1. Create gadget directory.
mkdir -p "${GADGET_DIR}"

# 2. Set device-level descriptors.
echo "${VID}" > "${GADGET_DIR}/idVendor"
echo "${PID}" > "${GADGET_DIR}/idProduct"
echo "${BCD_DEVICE}" > "${GADGET_DIR}/bcdDevice"
echo "${BCD_USB}" > "${GADGET_DIR}/bcdUSB"
echo "${DEVICE_CLASS}" > "${GADGET_DIR}/bDeviceClass"
echo "${DEVICE_SUB_CLASS}" > "${GADGET_DIR}/bDeviceSubClass"
echo "${DEVICE_PROTOCOL}" > "${GADGET_DIR}/bDeviceProtocol"

# 3. Set strings (optional but informative).
mkdir -p "${GADGET_DIR}/strings/0x409"
echo "USBX" > "${GADGET_DIR}/strings/0x409/manufacturer"
echo "USBX Virtual Device" > "${GADGET_DIR}/strings/0x409/product"
echo "0001" > "${GADGET_DIR}/strings/0x409/serialnumber"

# 4. Create FunctionFS function.
mkdir -p "${GADGET_DIR}/functions/${FFS_NAME}"

# 5. Create configuration and link function.
mkdir -p "${GADGET_DIR}/configs/${CONFIG_NAME}"
mkdir -p "${GADGET_DIR}/configs/${CONFIG_NAME}/strings/0x409"
echo "${CONFIG_LABEL}" > "${GADGET_DIR}/configs/${CONFIG_NAME}/strings/0x409/configuration"
echo "${MAX_POWER}" > "${GADGET_DIR}/configs/${CONFIG_NAME}/MaxPower"

ln -sf "${GADGET_DIR}/functions/${FFS_NAME}" "${GADGET_DIR}/configs/${CONFIG_NAME}/"

# 6. Mount FunctionFS.
mkdir -p "${FFS_DIR}"
if mountpoint -q "${FFS_DIR}" 2>/dev/null; then
    umount "${FFS_DIR}"
fi
mount -t functionfs "${FFS_NAME}" "${FFS_DIR}"

# 7. Bind to UDC.
echo "[*] Binding gadget to UDC: ${UDC}"
echo "${UDC}" > "${GADGET_DIR}/UDC"

echo ""
echo "[+] USBX gadget '${GADGET_NAME}' is ready."
echo "    VID=0x5553 PID=0x4258 bound to UDC=${UDC}"
echo "    FunctionFS mounted at: ${FFS_DIR}"
echo ""
echo "    Now run side-a on this machine:"
echo "      ./side-a -transport gadget -socks5 :1080"
echo ""
echo "    In the VM (with USB passthrough), run side-b:"
echo "      ./side-b -transport usb -usb-vid 0x5553 -usb-pid 0x4258"
echo ""
echo "    To remove: sudo ./scripts/setup-gadget.sh --cleanup"
