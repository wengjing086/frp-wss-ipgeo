# frp-wss-ipgeo v0.1.0

基于 frp v0.71.1 的补丁发布：

- ✅ **frps 直接接收 WSS**（WebSocket over TLS），无需前置 nginx / 第二个 frpc 做 TLS 终结；frpc 配置 `transport.protocol = "wss"` 同端口直连。
- ✅ **后台显示真实客户端 IP** + **国内归属地/运营商**：中国大陆 IPv4/IPv6 显示 `IP (省 市 运营商)`，境外与内网只显示 IP。
- ✅ 在线查询，无内置地址库；结果带内存 + 磁盘缓存、节流与限流熔断。

## 包含的二进制

| 文件 | 平台 | 说明 |
|---|---|---|
| `frps_linux_amd64` | Linux x86_64 | 服务端（WSS 补丁）|
| `frps_linux_arm64` | Linux AArch64 | 服务端（WSS 补丁，OpenWrt/树莓派等）|
| `frps_windows_amd64.exe` | Windows x86_64 | 服务端（WSS 补丁）|
| `frpc_linux_amd64` | Linux x86_64 | 客户端（官方）|
| `frpc_linux_arm64` | Linux AArch64 | 客户端（官方）|
| `frpc_windows_amd64.exe` | Windows x86_64 | 客户端（官方）|

## 用法

frps 照常配置 `bindPort`；frpc 增加 `transport.protocol = "wss"`。详见仓库 README。

> 归属地查询需 frps 能访问外网（ip138 / pconline / geojs）。
