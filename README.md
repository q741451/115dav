# 115dav

把 115 网盘挂成只读 WebDAV，给 Infuse / VidHub / nPlayer 这类播放器在线看片。

单个 Go 二进制，无运行时依赖。

## 快速开始

从 [Releases](https://github.com/q741451/115dav/releases) 下载对应平台的包，解压后：

```
115dav -cookie 'UID=...; CID=...; SEID=...; KID=...'
```

在播放器里添加 WebDAV 来源，地址填 `http://<这台机器的IP>:8115/`。

> **默认没有密码。** 能连到这个端口的人就能读你的整个网盘。放到局域网以外一定要加 `-user` / `-pass`。

## 拿 cookie

浏览器登录 115.com → F12 → Application → Cookies → `https://115.com`，取 **`UID`、`CID`、`SEID`、`KID` 四个**。

少了 `KID` 会收到「登录超时，请重新登录」——看着像 cookie 过期，其实只是没带全。

整段粘进去就行，无关的 cookie 会被忽略。命令行参数同机任何用户都能用 `ps` 看到，想避开就用环境变量：

```
PAN115_COOKIE='UID=...; CID=...; SEID=...; KID=...' 115dav
```

启动时会验证一次，cookie 不通就直接退出。

## 无人值守：从 cookie 服务器订阅

长期挂在路由器上时，cookie 过期就得上去重启。改成从 [cookie-sync](https://github.com/q741451/cookie-sync) 服务器读，浏览器插件在任何一台机器上重新上传，这边下一个请求自动接上：

```
115dav -cookie-server https://sync.example.com \
       -cookie-channel 我的频道 -cookie-key 频道key
```

给只读 key 就够。key 同样可以用 `PAN115_COOKIE_KEY` 传，避开 `ps`。和 `-cookie` / `PAN115_COOKIE` 二选一，同时给会报错。

- 115 拒绝当前 cookie 时自动回服务器重取；服务器上还是那份就不再试。
- 取不到可用 cookie 期间一律回 **503 + `Retry-After`**（默认 30 秒内不再打扰 115 和 cookie 服务器）。
- 换 cookie 会丢弃所有缓存——新 cookie 可能属于另一个账号。
- 用 `http://` 的话 key 和 cookie 明文过网络，启动时会警告。

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-cookie-server` | — | 服务器地址，或 `PAN115_COOKIE_SERVER` |
| `-cookie-channel` | — | 频道名，或 `PAN115_COOKIE_CHANNEL` |
| `-cookie-key` | — | 频道 key，或 `PAN115_COOKIE_KEY` |
| `-cookie-domain` | `115.com` | 读频道里的哪个 domain |
| `-cookie-retry` | `30s` | 取不到可用 cookie 后的静默期 |

## 参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` | `:8115` | 监听地址 |
| `-cookie` | — | 115 cookie，或 `PAN115_COOKIE` |
| `-user` / `-pass` | 空 | HTTP Basic 认证，或 `PAN115_USER` / `PAN115_PASS` |
| `-root` | `0` | 只挂某个子目录时填它的 115 目录 id |
| `-dir-ttl` | `30s` | 目录列表缓存时长 |
| `-link-ttl` | `2h` | 直链复用时长 |
| `-rate` | `2` | 115 API 每秒请求上限（不限制读文件） |
| `-page-size` | `1000` | 列目录分页大小，上限 1150 |
| `-ua` | 115 浏览器 | 发给 115 的 User-Agent |
| `-v` | 关 | 打印每个请求 |
| `-version` | — | 打印版本 |

## 你会遇到的行为

**同一个文件最多同时读两次。** 这是 115 的限制。第三个读会挤掉最早那个——被挤掉的一方收到 `503`，或者传输中途被截断，播放器通常会自动重连。

实际影响：一个人拖进度条完全正常（被挤掉的是他自己刚放弃的那个读）；两个人同时看**同一部**片会互相打断，看**不同**的片则互不影响。

**视频走本地转发，不是 302 跳转。** 115 的直链和取链时的 UA 绑定，直接把播放器丢过去会吃 403。代价是流量过一遍本机。

**只读。** 所有写方法一律 405。

**目录很大会很慢。** 受 115 每秒 2 个请求限制，一万个文件的目录大约要 5 秒列完，五万个约 25 秒——而 WebDAV 协议不支持分页，播放器必须一次收完。单目录别堆太多文件。

**列目录失败会报错，不会返回空目录。** 播放器把空目录当成「片库被删了」。

**同目录重名文件**显示成 `片子 (2).mkv`。

**`Depth: infinity` 的 PROPFIND 一律 403。** 正常客户端都用 `Depth: 1`。

## 构建

```
go build -o 115dav .
go test ./...
```

需要 Go 1.22，依赖只有 `golang.org/x/{sync,time}`。

交叉编译全部平台（产物在 `dist/`）：

```
VERSION=v0.1.0 scripts/build-all.sh
```

编译环境定义在 `.github/builder/Dockerfile`（Ubuntu 按 digest 钉死 + 校验过的 Go），同一个 commit 在不同机器上编出的二进制逐字节相同。

Go 特意压在 1.22 以控制体积，代价是它已过维护期，二进制里的 `crypto/tls` 和 `net/http` 不再收安全补丁——把端口开到可信网络之外前请掂量。

## 状态

已在真实 115 账号上验证：列目录、`HEAD`、带 `Range` 的 `GET`、直链取用与加解密均正常。

协议照着 115 网页版自己实现，不依赖第三方 SDK。遇到问题请带 `-v` 跑一遍，把日志发到 issue。
