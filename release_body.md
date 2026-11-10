# frp-wss-ipgeo v0.1.0

基于 **frp v0.71.1** 的补丁版本，让 frps 直接接收 **WSS（WebSocket over TLS）** 连接，并在后台显示中国大陆客户端的 **真实 IP + 归属地（省 市）+ 运营商**。

## 核心特性
- **frps 直收 WSS**：无需在前面挂 nginx，frps 在同端口（bindPort）上自动识别 `GET /~!frp` 的 WSS 流量并升级。frpc 端零改动，配置 `transport.protocol = "wss"` 即可。
- **真实客户端 IP**：修复了 x/net/websocket 服务端把 RemoteAddr 误报为 Origin 的问题，后台显示真实 TCP 地址。
- **中国大陆 IP 归属地 + 运营商**：在线查询（ip138/ipshudi 主源，geojs 仅运营商兜底），无内置数据库。仅中国大陆显示 `(省 市 运营商)`，境外与内网原样显示。
- **IPv4 + IPv6 双支持**。

## 预编译二进制（见下方 Assets）
| 文件 | 平台 | 说明 |
|------|------|------|
| frps_linux_amd64 | Linux x86_64 | 服务端（含 WSS 补丁 + 归属地） |
| frps_linux_arm64 | Linux ARM64 | 服务端（OpenWrt/树莓派等） |
| frps_windows_amd64.exe | Windows x86_64 | 服务端（自测用） |
| frpc_linux_amd64 | Linux x86_64 | 客户端（官方，支持 wss） |
| frpc_linux_arm64 | Linux ARM64 | 客户端 |
| frpc_windows_amd64.exe | Windows x86_64 | 客户端 |

所有二进制均为 `CGO_ENABLED=0` 静态编译，可直接运行，无需额外依赖。

## 使用方式
- **直接用预编译二进制**：下载对应平台的 `frps`/`frpc`，按官方文档配置，客户端加一行 `transport.protocol = "wss"`。
- **自己打补丁编译**：`git clone` 本仓库，参考 `README.md` 与 `apply.sh`，把 `frp-patch/` 下的文件覆盖到官方 frp v0.71.1 源码后 `go build`。

## 注意
- frps 需要能访问外网才能查询 IPv6 归属地（ip138 主源）。
- frpc 的 wss 直连目前不支持 `serverAddr` 填 IPv6（frp 上游限制）。

许可证：Apache-2.0（同 frp 上游）。
