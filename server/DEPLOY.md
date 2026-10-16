# 服务端部署（Windows / Linux）

目前服务端由两个进程组成：`lanroom-server` 提供建房、成员状态和文字聊天 API；[SoftEther VPN Server](https://www.softether.org/) 承载房间内的二层网络流量。每个房间对应一个独立 Virtual Hub，TCP、UDP 和局域网广播由虚拟网卡进入 Hub 后转发。`lanroom-server` 用同机 `vpncmd` 创建 Hub 和成员账号。

## 1. 安装 SoftEther VPN Server

按 [Windows 安装说明](https://www.softether.org/4-docs/1-manual/3/3.1) 或 [Linux 安装说明](https://www.softether.org/4-docs/1-manual/3/3.2) 安装 **VPN Server**，并设置服务器管理员密码。确认 VPN 客户端可访问其监听端口，并记下同机管理端口（示例使用 `127.0.0.1:5555`）。确保同机可运行 `vpncmd`。

`lanroom-server` 目前只管理单机版 SoftEther VPN Server，不能管理 VPN Bridge 或集群节点。保留 SoftEther 配置文件的备份。

## 2. 构建服务端

需要 Go 1.24 或更新版本。在 Windows PowerShell 中：

```powershell
cd server
go build -o dist/windows-amd64/lanroom-server.exe ./cmd/lanroom-server
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o dist/linux-amd64/lanroom-server ./cmd/lanroom-server
Remove-Item Env:GOOS
Remove-Item Env:GOARCH
```

也可以在 Linux 主机上直接执行 `go build -o lanroom-server ./cmd/lanroom-server`。

## 3. 启动

这些环境变量在两个平台上相同：

| 变量 | 用途 |
| --- | --- |
| `LANROOM_ADDR` | HTTP 监听地址；默认 `:8080` |
| `LANROOM_STATE_FILE` | 房间状态文件；默认当前目录的 `lanroom-state.json` |
| `LANROOM_CHAT_ONLY` | 设为 `1` 时仅启动房间与聊天，用于开发调试；不得与 VPN 配置一起使用 |
| `LANROOM_VPNCMD` | 本机 `vpncmd` 的完整路径 |
| `LANROOM_VPN_ADMIN_ADDRESS` | SoftEther 本机管理地址；默认 `127.0.0.1:5555` |
| `LANROOM_VPN_ADMIN_PASSWORD` | SoftEther 服务器管理员密码 |
| `LANROOM_VPN_PUBLIC_HOST` | Windows 客户端可访问的 VPN 域名或 IP |
| `LANROOM_VPN_PUBLIC_PORT` | VPN 客户端端口；默认 `443` |

Windows PowerShell 示例（按实际路径和域名调整）：

```powershell
$env:LANROOM_ADDR = "127.0.0.1:8080"
$env:LANROOM_STATE_FILE = "C:\ProgramData\LanRoom\rooms.json"
$env:LANROOM_VPNCMD = "C:\Program Files\SoftEther VPN Server\vpncmd.exe"
$env:LANROOM_VPN_ADMIN_ADDRESS = "127.0.0.1:5555"
$env:LANROOM_VPN_PUBLIC_HOST = "vpn.example.com"
$env:LANROOM_VPN_PUBLIC_PORT = "443"
$secret = Read-Host "SoftEther 管理员密码" -AsSecureString
$env:LANROOM_VPN_ADMIN_PASSWORD = [System.Net.NetworkCredential]::new("", $secret).Password
.\dist\windows-amd64\lanroom-server.exe
```

Linux Bash 示例：

```bash
export LANROOM_ADDR=127.0.0.1:8080
export LANROOM_STATE_FILE=/var/lib/lanroom/rooms.json
export LANROOM_VPNCMD=/usr/local/vpnserver/vpncmd
export LANROOM_VPN_ADMIN_ADDRESS=127.0.0.1:5555
export LANROOM_VPN_PUBLIC_HOST=vpn.example.com
export LANROOM_VPN_PUBLIC_PORT=443
read -rs -p 'SoftEther 管理员密码: ' LANROOM_VPN_ADMIN_PASSWORD
echo
export LANROOM_VPN_ADMIN_PASSWORD
./dist/linux-amd64/lanroom-server
```

服务启动时会用 `vpncmd HubList` 检查管理连接。成功后 `GET /healthz` 返回 `"vpn_enabled": true`。没有 VPN 配置时，服务端会明确报错并退出；只有显式设置 `LANROOM_CHAT_ONLY=1` 才提供无组网的房间和聊天，`vpn_enabled` 为 `false`。
从纯聊天模式切换到 VPN 模式时，请使用新的状态文件；已有纯聊天房间没有对应的 SoftEther Hub，服务端会拒绝把两种状态混用。

## 4. 客户端连接资料

创建或加入房间的 API 响应中，`vpn` 字段会包含服务器、端口、Hub、用户名、密码及房间子网。每位成员有单独账号。服务端为每个 Hub 配置 DHCP，不下发默认网关和 DNS，因此不会接管客户端的默认上网路由。

当前 Windows 应用只完成房间与文字聊天，还没有自动创建/连接 SoftEther 虚拟网卡。要验证二层网络，需在两台 Windows 电脑上用 SoftEther VPN Client 手动创建虚拟网卡并用各自的 `vpn` 凭据连接，再检查房间网段地址、TCP/UDP 互通与广播发现。

## 当前部署限制

- API 尚无注册登录、建房权限、限流和房间删除功能。请只在可信环境内使用；对公网开放前需要补齐这些保护。
- 房间状态文件保存加盐的密码派生值和成员令牌，请限制文件访问权限并备份。文字聊天最近 100 条只存在内存中。旧版房间的邀请码在升级后仍可用作房间密码。
- 服务端会通过本机进程参数调用 `vpncmd`，管理员和 VPN 凭据可能被有权查看本机进程的用户读取。将服务运行在受信任的主机上。
- 服务端与 SoftEther 的联动尚未在实际安装环境中验证；本地已完成跨平台编译。崩溃发生在 Hub 创建与状态文件写入之间时，可能留下需要管理员手动清理的 `LR...` Hub。
- 语音、屏幕共享、系统音频和 2160p/120fps 媒体转发还未实现。
