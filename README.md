# USBX — Virtual USB Channel SOCKS5 Proxy

通过虚拟机 USB 直通通道实现的 SOCKS5 代理系统。A 端插入一个 USB 设备（物理或虚拟），
Hypervisor 将该设备映射给 VM 中的 B 端，两者通过 USB 批量端点进行双向通信。
A 端运行 SOCKS5 代理，所有代理请求经 USB 通道封装转发至 B 端，由 B 端访问互联网并返回。

## 架构

```
┌─────────── A (Host: Windows / Linux) ────────────────────────────┐
│                                                                     │
│  ┌───────────────────────────────────────┐                         │
│  │  SOCKS5 Server (localhost:1080)       │  ← 浏览器 / curl        │
│  │  RFC 1928, CONNECT command            │                         │
│  └────────────┬──────────────────────────┘                         │
│               │                                                    │
│  ┌────────────▼──────────────────────────┐                         │
│  │  Stream Mux (channel/mux.go)          │                         │
│  │  - 多路复用，StreamID 分用            │                         │
│  │  - STREAM_OPEN / ACK 握手             │                         │
│  │  - 数据分帧 (max 65535 bytes)         │                         │
│  └────────────┬──────────────────────────┘                         │
│               │                                                    │
│  ┌────────────▼──────────────────────────┐                         │
│  │  USB Transport (usbdev/)              │                         │
│  │  - USB 模式: libusb → EP2 OUT         │──┐                      │
│  │  - USB 模式: libusb ← EP1 IN          │  │                      │
│  │  - TCP 模式: TCP socket (开发测试)    │  │                      │
│  │  - Gadget 模式: FunctionFS (Linux)     │  │                      │
│  └───────────────────────────────────────┘  │                      │
│                                              │  USB 物理连接         │
└──────────────────────────────────────────────┼──────────────────────┘
                                               │
                    ┌──────────────────────────┐
                    │  USB Device (Gadget)     │
                    │  VID=0x5553 PID=0x4258  │
                    │  EP1 (0x81) Bulk IN      │
                    │  EP2 (0x02) Bulk OUT     │
                    │  内部数据缓冲区          │
                    └──────────┬───────────────┘
                               │  Hypervisor USB 直通
┌──────────────────────────────┼────── B (VM: Windows / Linux) ─────────────┐
│  ┌───────────────────────────▼──────────┐                          │
│  │  USB Transport (usbdev/)              │                          │
│  │  - USB 模式: libusb ← EP2 OUT        │                          │
│  │  - USB 模式: libusb → EP1 IN         │                          │
│  └────────────┬──────────────────────────┘                          │
│               │                                                     │
│  ┌────────────▼──────────────────────────┐                          │
│  │  Stream Demux (channel/mux.go)        │                          │
│  │  - STREAM_OPEN → acceptCh             │                          │
│  └────────────┬──────────────────────────┘                          │
│               │                                                     │
│  ┌────────────▼──────────────────────────┐                          │
│  │  Forwarder (forwarder/)               │                          │
│  │  - 读取目标地址，net.Dial 互联网      │                          │
│  │  - 成功 → AcceptStream(STREAM_ACK)    │                          │
│  │  - 失败 → RejectStream(STREAM_ERROR)  │                          │
│  │  - 双向 io.Copy 中继                  │                          │
│  └────────────┬──────────────────────────┘                          │
│               │                                                     │
│          Internet 🌐                                                │
└─────────────────────────────────────────────────────────────────────┘
```

**核心思想**：USB 设备充当 A 和 B 之间的共享通信通道。A 写入 EP2 OUT 的数据被 USB 设备缓冲，
B 通过 VM 直通读取该数据并转发到互联网；B 将响应写入 EP1 IN，A 读取后返回给 SOCKS5 客户端。

## 项目结构

```
usbx/
├── go.mod
├── driver/
│   ├── build.bat                    # UMDF 驱动编译脚本 (Windows)
│   └── umdf/
│       ├── driver.c                 # UMDF2 驱动入口
│       ├── device.c                 # USB 设备模拟 + IOCTL 控制设备
│       ├── driver.h                 # 公共定义 (VID/PID/IOCTL 码)
│       ├── device.h                 # 设备 API
│       ├── trace.h                  # 调试日志
│       └── usbx.inx               # INF 安装模板
├── scripts/
│   └── setup-gadget.sh             # Linux USB Gadget 配置脚本
├── cmd/
│   ├── side-a/main.go              # A 端：SOCKS5 代理 + TCP/USB/IOCTL/Gadget 传输
│   └── side-b/main.go              # B 端：USB 设备处理 + 互联网转发
├── internal/
│   ├── protocol/
│   │   ├── frame.go                # 线协议帧格式、编解码、帧池
│   │   └── frame_test.go           # 单元测试 + 基准测试
│   ├── usbdev/
│   │   ├── interface.go            # Transport 接口 + 公共类型
│   │   ├── device.go               # USB 描述符 (VID/PID/端点)
│   │   ├── tcp_transport.go        # TCP 传输实现 (开发/测试)
│   │   ├── ioctl_transport_windows.go  # IOCTL 传输 (Windows)
│   │   ├── gadget_transport_linux.go   # FunctionFS Gadget 传输 (Linux)
│   │   ├── gadget_transport_other.go   # Gadget 传输桩 (非 Linux)
│   │   ├── usb_transport_cgo.go    # USB 传输实现 (需 cgo + libusb)
│   │   └── usb_transport_nocgo.go  # USB 传输桩 (cgo 不可用时)
│   ├── channel/
│   │   └── mux.go                  # 流多路复用器
│   ├── socks5/
│   │   └── server.go               # SOCKS5 代理 (RFC 1928)
│   └── forwarder/
│       └── forwarder.go            # B 端互联网转发器
└── README.md
```

### 模块职责

| 模块 | 职责 |
|------|------|
| `protocol` | USB 仿真线协议帧格式，帧编解码，sync.Pool 缓冲池 |
| `usbdev` | Transport 接口 + TCP/USB/IOCTL 三种传输实现 + USB 描述符 |
| `channel` | 流多路复用器，StreamID 分用，ACK 握手 |
| `socks5` | RFC 1928 SOCKS5 服务端，CONNECT 命令，隧道化至 USB 通道 |
| `forwarder` | B 端互联网转发器：接收流 → 拨号目标 → 双向中继 |

## 高延迟优化（USB over Network 20-50ms）

针对 USB over Network 映射引入的 20-50ms 延迟，实现了三层优化：

### 1. 乐观 STREAM_OPEN（节省 1 RTT）

```
优化前 (2 RTT):
  A ──STREAM_OPEN──→ B                ← 20-50ms
  A ←──STREAM_ACK── B                ← 20-50ms
  A ──SOCKS5 reply──→ Client
  A ──STREAM_DATA──→ B

优化后 (1 RTT):
  A ──STREAM_OPEN──→ B                ← 20-50ms
  A ──SOCKS5 reply──→ Client          (立即回复，不等 ACK)
  A ──STREAM_DATA──→ B                (数据立即开始发送)
  A ←──STREAM_ACK── B                 (ACK 异步到达)
```

`socks5.Server` 使用 `OpenStreamPipeline` 代替 `OpenStream`，不等 B 端确认即回复客户端成功并开始转发数据。若 B 端连接失败（STREAM_ERROR 异步到达），立即关闭客户端连接。节省一个完整往返延迟。

### 2. 写缓冲合并（减少传输次数）

```
优化前: 每个 io.Copy 的 write → 立即编码帧 → USB bulk transfer (20-50ms × N)
优化后: 多次 write → 累积到 32KB 缓冲区 → 5ms 定时器触发 → 一个大帧 → 1 次 USB transfer
```

`Stream.Write()` 将数据先写入内部缓冲区，后台 flush 协程在以下条件触发合并发送：
- 缓冲区达到 32KB
- 距离首次缓冲已过 5ms
- 流关闭时强制 flush

对小包密集的 HTTP 流量，传输次数可降低 10-100 倍。

### 3. 双向独立中继 + 异步失败检测

SOCKS5 处理器启动 3 个协程：
- 客户端→Stream (A→B 数据转发)
- Stream→客户端 (B→A 数据转发)  
- ACK 监听协程（检测 B 端异步连接失败，立即断开客户端）

B 端 Forwarder 确保 B→A 方向数据在流关闭前完全 flush，避免管道尾部数据丢失。

## 技术栈

| 层次 | 技术 |
|------|------|
| 语言 | Go 1.21+ |
| USB 传输 | libusb-1.0 (via gousb), WinUSB 驱动 |
| IOCTL 传输 | Windows UMDF2 虚拟 USB 驱动 |
| Gadget 传输 | Linux ConfigFS + FunctionFS USB Gadget |
| TCP 传输 | net.Dial / net.Listen (开发测试) |
| 多路复用 | 自定义帧协议 (16 字节头 + 0~64KB payload) |
| 代理协议 | SOCKS5 (RFC 1928) |
| 并发模型 | Goroutine-per-stream, sync.Pool 复用 |
| USB 设备 | 需配套 USB Gadget 固件 (如 Raspberry Pi Pico) |

## 传输模式

### TCP 模式（开发 / 测试，跨平台）

A 和 B 通过 TCP 直连。无需 USB 硬件，用于本地开发调试。

```bash
# 任意平台
side-a -transport tcp -usb-channel <B_IP>:9000
side-b -transport tcp -listen :9000
```

### IOCTL 模式（Windows 纯软件虚拟 USB）

**无需任何 USB 硬件**。A 端加载一个 UMDF2 驱动，凭空在 Windows 上创建虚拟 USB 设备
(VID=0x5553, PID=0x4258)，Hypervisor 将该虚拟设备直通给 VM B。A 端 Go 程序通过
IOCTL 与驱动通信。

```
┌─── Windows A (Host) ──────────────────────┐
│                                             │
│  Go side-a ──IOCTL──→ UMDF Driver           │
│              (WRITE)   ├─ Ep2Out ring buffer │
│              (READ)    ├─ Ep1In ring buffer  │
│                        └─ USB device emulation│
│                               │               │
│   Hypervisor USB passthrough  │               │
└───────────────────────────────┼───────────────┘
                                │
┌───────────────────────────────┼───────────────┐
│  VM B sees USB device:       │               │
│  side-b -transport usb       │               │
│  (WinUSB/libusb)             │               │
│   read EP1 IN ← Ep2Out      │               │
│   write EP2 OUT → Ep1In     │               │
└───────────────────────────────────────────────┘
```

**前提条件**：
1. Visual Studio 2022 + Windows Driver Kit (WDK)
2. 开启测试签名模式：`bcdedit /set testsigning on`（需重启）

**构建和安装驱动**：
```bash
# 在 "x64 Native Tools Command Prompt for VS 2022" 中
cd driver
build.bat

# 安装驱动
pnputil /add-driver usbx.inf /install

# 验证
pnputil /enum-drivers | findstr USBX
# → 设备管理器中可见 "USBX Virtual USB Device" (VID=5553, PID=4258)
```

**运行**：
```bash
# A 端 (驱动已安装)
side-a -transport ioctl -socks5 :1080

# B 端 (VM，Hypervisor USB 直通后可见设备)
CGO_ENABLED=1 go build ./cmd/side-b/
side-b -transport usb -usb-vid 0x5553 -usb-pid 0x4258
```

### Gadget 模式（Linux 纯软件虚拟 USB）

**无需任何 USB 硬件**。A 端通过 Linux 内核 ConfigFS + FunctionFS 创建虚拟 USB Gadget 设备
(VID=0x5553, PID=0x4258)，Hypervisor 将该设备直通给 VM B。A 端 Go 程序通过
FunctionFS 端点读写数据。

```
┌─── Linux A (Host) ───────────────────────────┐
│                                                │
│  Go side-a ──write──→ /dev/usb-ffs/usbx/ep1   │
│              ←──read── /dev/usb-ffs/usbx/ep2   │
│                       │                        │
│              ConfigFS USB Gadget               │
│              (VID=0x5553 PID=0x4258)           │
│                       │                        │
│  Hypervisor USB passthrough                    │
└───────────────────────┼────────────────────────┘
                        │
┌───────────────────────┼────────────────────────┐
│  VM B sees USB device:                         │
│  side-b -transport usb                         │
│  (gousb/libusb)                                │
│   read EP1 IN ← A-side data                   │
│   write EP2 OUT → A-side data                 │
└────────────────────────────────────────────────┘
```

**前提条件**：
1. Linux 内核支持 `CONFIG_USB_GADGET` + `CONFIG_USB_FUNCTIONFS`
2. root 权限（运行 setup script）
3. 可选：`dummy_hcd` 模块用于虚拟 UDC（无需物理 USB 控制器）

**配置和运行**：
```bash
# 1. 安装 libusb 依赖（仅 USB 模式编译时需要）
sudo apt install libusb-1.0-0-dev

# 2. 创建虚拟 USB Gadget（需 root）
sudo ./scripts/setup-gadget.sh
# 如果需指定 UDC：sudo ./scripts/setup-gadget.sh dummy_hcd.0
# 没有 dummy_hcd 则：sudo modprobe dummy_hcd

# 3. A 端（Gadget 已配置）
go build -o side-a ./cmd/side-a/
./side-a -transport gadget -socks5 :1080

# 4. B 端（VM，Hypervisor USB 直通后可见设备）
CGO_ENABLED=1 go build -o side-b ./cmd/side-b/
side-b -transport usb -usb-vid 0x5553 -usb-pid 0x4258

# 5. 清理
sudo ./scripts/setup-gadget.sh --cleanup
```

### USB 模式（需物理 Gadget 设备）

A 通过 libusb 直接读写本地 USB 设备的批量端点。
B（在 VM 中）通过 Hypervisor USB 直通读写同一设备的端点。

**前提条件**：
1. 一个 USB Gadget 设备（如 Raspberry Pi Pico），烧录配套固件
2. USB 设备描述符：VID=0x5553, PID=0x4258
3. EP1 (0x81) Bulk IN — B→A 方向（互联网响应）
4. EP2 (0x02) Bulk OUT — A→B 方向（代理请求）
5. Windows 上安装 WinUSB 或 libusb-win32 驱动
6. 编译时需 `CGO_ENABLED=1` + libusb 开发库

```bash
# 1. 安装 libusb 依赖
# Windows: 下载 libusb-win32 或通过 MSYS2/vcpkg
# Linux:   apt install libusb-1.0-0-dev

# 2. 拉取 gousb 依赖
go get github.com/google/gousb@latest

# 3. 编译（启用 cgo）
CGO_ENABLED=1 go build -o side-a.exe ./cmd/side-a/
CGO_ENABLED=1 go build -o side-b.exe ./cmd/side-b/

# 4. 运行
# A 端 (USB 设备物理连接在此机器):
side-a -transport usb -usb-vid 0x5553 -usb-pid 0x4258 -socks5 :1080

# B 端 (VM，通过 Hypervisor USB 直通获取设备):
side-b -transport usb -usb-vid 0x5553 -usb-pid 0x4258
```

## 线协议

### 帧格式 (Big-Endian)

```
 0               4       5   6       8              12             16
┌───────────────┬───────┬────┬───────┬──────────────┬──────────────┬────────┐
│ Magic (4B)    │Ver(1B)│Type│Flags  │ StreamID(4B) │ Length (4B)  │Payload │
│ 0x55534258    │ 0x01  │(1B)│(2B)   │              │              │0~64KB  │
└───────────────┴───────┴────┴───────┴──────────────┴──────────────┴────────┘
  Header = 16 bytes                              Payload = 0 ~ 65535 bytes
```

### 帧类型

| 类型码 | 名称 | 方向 | 说明 |
|--------|------|------|------|
| 0x10 | STREAM_OPEN | A→B | 打开流，payload=目标地址 |
| 0x11 | STREAM_OPEN_ACK | B→A | 流已建立 |
| 0x12 | STREAM_DATA | 双向 | 流数据 |
| 0x13 | STREAM_CLOSE | 双向 | 关闭流 |
| 0x14 | STREAM_CLOSE_ACK | 双向 | 确认关闭 |
| 0x15 | STREAM_ERROR | B→A | 流错误 |
| 0xFE | PING | A→B | 保活探测 |
| 0xFF | PONG | B→A | 保活响应 |

### 流生命周期

```
A-Side                              B-Side
  │── STREAM_OPEN(host:port) ──────→│
  │                                  │── net.Dial(target)
  │←── STREAM_OPEN_ACK ────────────│
  │                                  │
  │   SOCKS5 回复成功                │
  │── STREAM_DATA(payload) ────────→│── target.Write()
  │←── STREAM_DATA(payload) ───────│── target.Read()
  │       ... (双向)                │
  │── STREAM_CLOSE ────────────────→│── target.Close()
  │←── STREAM_CLOSE_ACK ───────────│
```

## USB Device 通信模型

```
A 端 (USB Host)                  USB Device (Gadget)               B 端 (USB Host, VM)

  ① 发送代理请求:
  WriteFrame()                    ┌──────────────────┐
  ──── EP2 OUT ───────────────→  │  OUT buffer      │
                                  │                  │
  ② 接收互联网响应:               │  IN buffer       │
  ←── EP1 IN ─────────────────  │                  │
  ReadFrame()                    └──────────────────┘
                                                                   ③ 读取代理请求:
                                                                   ←── EP2 OUT ───────────
                                                                   ReadFrame()

                                                                   ④ 发送互联网响应:
                                                                   WriteFrame()
                                                                   ──── EP1 IN ──────────→
```

**端点方向说明** (USB 视角):
- **EP1 (0x81) Bulk IN**: Device→Host。B 写，A 读。用于传递互联网响应。
- **EP2 (0x02) Bulk OUT**: Host→Device。A 写，B 读。用于传递代理请求。

两端都作为 USB Host 运行，通过 Gadget 设备的内部缓冲实现数据交换。

## 部署

### 快速开始 (TCP 模式)

```bash
# 编译
go build -o side-a.exe ./cmd/side-a/
go build -o side-b.exe ./cmd/side-b/

# B 端 (VM, 能访问互联网)
./side-b.exe -transport tcp -listen 0.0.0.0:9000

# A 端 (需要代理的机器)
./side-a.exe -transport tcp -usb-channel <B_IP>:9000 -socks5 :1080
```

### 使用代理

```bash
# curl HTTP
curl --socks5 127.0.0.1:1080 http://example.com/

# curl HTTPS (Linux/macOS)
curl --socks5 127.0.0.1:1080 https://httpbin.org/ip

# 浏览器: 设置 SOCKS5 代理 → 127.0.0.1:1080
```

### USB 模式部署

1. **准备 USB Gadget 设备**（如 Raspberry Pi Pico），烧录配套固件
2. **在 Windows A** 上安装 WinUSB 驱动（通过 Zadig 工具）
3. **编译**：`CGO_ENABLED=1 go build -o side-a.exe ./cmd/side-a/`
4. **启动 A 端**：`side-a -transport usb -socks5 :1080`
5. **在 Hypervisor 中**将 USB 设备直通给 VM B
6. **在 VM B 中**安装 WinUSB 驱动，编译并启动：`side-b -transport usb`
7. **配置浏览器**使用 `socks5://127.0.0.1:1080`

### 命令行参数

| 参数 | 适用端 | 默认值 | 说明 |
|------|--------|--------|------|
| `-transport` | A, B | `tcp` | 传输模式：`tcp` / `usb` / `ioctl` (Windows) / `gadget` (Linux) |
| `-usb-channel` | A | `127.0.0.1:9000` | B 端 TCP 地址 (tcp 模式) |
| `-listen` | B | `0.0.0.0:9000` | TCP 监听地址 (tcp 模式) |
| `-usb-vid` | A, B | `0x5553` | USB Vendor ID (usb 模式) |
| `-usb-pid` | A, B | `0x4258` | USB Product ID (usb 模式) |
| `-socks5` | A | `127.0.0.1:1080` | SOCKS5 监听地址 |
| `-gadget-dir` | A | `/dev/usb-ffs/usbx` | FunctionFS 挂载路径 (gadget 模式) |

## 性能

- 帧编解码：~100 ns/encode, ~93 ns/decode (AMD Ryzen 7 7840HS)
- 单帧最大 payload：65535 字节
- 帧池 + 缓冲池：sync.Pool 复用，减少 GC
- 并发：goroutine-per-stream，流间无锁竞争
- TCP_NODELAY：禁用 Nagle 算法，降低延迟

## 配套 USB Gadget 固件

USB 模式需要一个物理 USB 设备作为通信桥梁。适合的硬件：
- **Raspberry Pi Pico** (RP2040, ~$4) — 推荐，USB 1.1
- **STM32F4 / STM32F7** — USB 2.0 High-Speed
- **ESP32-S2 / ESP32-S3** — 内置 USB OTG

固件功能：创建 VID=0x5553 PID=0x4258 的 USB 设备，提供 2 个批量端点，将 EP2 OUT 收到的数据内部转发到 EP1 IN 缓冲区。

## 许可

MIT License
