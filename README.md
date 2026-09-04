# 115dav

把 115 网盘挂成只读 WebDAV，给 Infuse / VidHub / nPlayer 这类播放器在线看片。

单个 Go 二进制，没有运行时依赖。

## 快速开始

从 [Releases](https://github.com/q741451/115dav/releases) 下载对应平台的包，解压后：

```
115dav -cookie 'UID=...; CID=...; SEID=...'
```

在 Infuse 里添加 WebDAV 来源，地址填 `http://<这台机器的IP>:8115/`。

> **默认没有密码**，能连到这个端口的人就能读你的网盘。放到局域网以外一定要加 `-user` / `-pass`。

## 拿 cookie

浏览器登录 115.com → F12 → Application → Cookies → `https://115.com`，取 `UID`、`CID`、`SEID`、`KID` 四个值。**四个都要**——少了 `KID`，115 会回「登录超时，请重新登录」，看起来像 cookie 过期，其实只是没带全。

整段粘进去就行，无关的 cookie 会被忽略。不想让密钥出现在进程参数里，用文件或环境变量：

```
115dav -cookie-file ~/.115cookie
PAN115_COOKIE='UID=...; CID=...; SEID=...' 115dav
```

启动时会先验证 cookie，不通就直接退出，不会等到播放器连上才发现。

## 无人值守：从 cookie 服务器订阅

挂在路由器上长期跑的话，cookie 过期就得上去重启一次。改成从 [cookie-sync](https://github.com/q741451/cookie-sync) 服务器读，浏览器插件在任何一台机器上重新上传，这边下一个请求就自己捡起来，不用重启：

```
115dav -cookie-server https://sync.example.com \
       -cookie-channel 我的频道 -cookie-key-file /etc/115dav.key
```

给只读 key 就够，它只会读，不会往上写。

和 `-cookie` / `-cookie-file` / `PAN115_COOKIE` 二选一，同时给会直接报错——两种模式出错时的行为不一样，让它自己猜不如说清楚。

行为：

- 启动只从服务器取一次，**不验 115**。服务器上放着过期 cookie 本来就是这个模式要处理的常态，等播放器自己发现就行。频道名、key、domain 写错会当场退出，并告诉你这个频道里实际有哪些 domain。
- 服务器连不上不影响启动（路由器开机时网络往往还没好），只 warn 一句，第一个请求再取。
- 115 说 cookie 过期 → 回服务器重取一次 → 拿到新的就换上继续；服务器上还是刚被拒的那份就不去试了。
- 换 cookie 会清掉所有缓存（目录、直链）。下次可能整个换了一个账号，旧的目录 id 和提取码没有任何意义。
- 取不到可用 cookie 的这段时间一律回 **503 + `Retry-After`**，默认 30 秒内不再碰 115 也不碰 cookie 服务器（`-cookie-retry` 可调）。回 503 而不是空目录是有意的：播放器看到空目录会以为片库被删了。
- 每次取都重新解析域名，服务器换 IP 不用重启。

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-cookie-server` | — | cookie-sync 服务器地址，也可用 `PAN115_COOKIE_SERVER` |
| `-cookie-channel` | — | 频道名，也可用 `PAN115_COOKIE_CHANNEL` |
| `-cookie-key` / `-cookie-key-file` | — | 频道 key，也可用 `PAN115_COOKIE_KEY` |
| `-cookie-domain` | `115.com` | 读频道里的哪个 domain |
| `-cookie-retry` | `30s` | 取不到可用 cookie 后，多久之内直接回 503 |

用 `http://` 而不是 `https://` 的话，key 和 115 cookie 都是明文过网络，启动时会警告一次。

## 参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` | `:8115` | 监听地址 |
| `-cookie` / `-cookie-file` | — | 115 cookie，也可用 `PAN115_COOKIE` |
| `-user` / `-pass` | 空 | HTTP Basic 认证，也可用 `PAN115_USER` / `PAN115_PASS` |
| `-root` | `0` | 只挂某个子目录时填它的 115 目录 id |
| `-dir-ttl` | `30s` | 目录列表缓存时长 |
| `-link-ttl` | `2h` | 直链复用时长 |
| `-rate` | `2` | 115 API 每秒请求数上限（不限制读文件） |
| `-page-size` | `1000` | 列目录分页大小，上限 1150 |
| `-v` | 关 | 打印每个请求 |
| `-version` | — | 打印版本 |

## 行为说明

- **视频走本地转发，不是 302 跳转。** 115 的直链跟取链时用的 UA 绑定，直接把播放器丢过去会吃 403。转发的代价是流量过一遍本机，好处是拖进度条正常。
- **只读。** 所有写方法一律 405，底层也没有实现任何 115 写接口。
- **直链过期自动重取**，播放不中断。
- **同目录重名文件**会自动显示成 `片子 (2).mkv`，保证每个都能访问。
- **列目录失败就报错，不会返回空目录。** 上游出错时返回半截 207 会被播放器读成「片库空了」，所以宁可给 503 / 502。
- **同一个文件在 PROPFIND / HEAD / GET 里的 ETag、修改时间、Content-Type 一致**（都取自目录信息，不透传 CDN 那一套）。`If-Range` 也在本地判定：那个校验值是我们发的，CDN 不认识，转过去只会换来一整个文件。
- **上游连接失败会自动重试一次**（还没往客户端写任何东西，GET 又不改变什么），家里上行抖一下不至于直接放不了。
- **缓存按条目数封顶**，不是按目录数：媒体库扫描几分钟就能走几千个目录，撑爆内存的是里面的条目。
- **`Depth: infinity` 的 PROPFIND 一律 403**（`propfind-finite-depth`）。整个账号递归列一遍要按每秒两个请求慢慢爬几个小时，正常客户端都用 `Depth: 1`。

## 构建

```
go build -o 115dav .
go test ./...
```

需要 Go 1.22。依赖只有 `golang.org/x/{net,sync,time}`。

交叉编译全部平台（和 CI 用同一份脚本，产物在 `dist/`）：

```
VERSION=v0.1.0 scripts/build-all.sh
```

## CI

push 自动测试并交叉编译十个平台；push `v*` tag 自动发 Release；也可以在 Actions 页面手动 Run workflow 并勾选发布。

编译环境全部定义在 `.github/builder/Dockerfile`：Ubuntu 22.04 按 digest 钉死，里面装指定版本并校验过 sha256 的 Go。编译器不在宿主机上跑，所以 CI 一律用 `ubuntu-latest`（GitHub 唯一不会删的标签），环境也不会跟着它动。

Go 特意压在 1.22（源码能编的最低版本）——之后每个版本产物都更大，1.22 比 1.27 小约 1.5 MB。代价是它已过维护期，二进制里的 `crypto/tls` 和 `net/http` 不再收安全补丁，把端口开到可信网络之外前请掂量。

构建是可复现的：同一个 commit 在不同机器上编，二进制和归档逐字节相同。升级 Ubuntu 或 Go 都只改 Dockerfile 里对应那几行。

## 状态

已在真实 115 账号上跑通：列目录（PROPFIND 207）、`HEAD`、`GET` 带 `Range` 从文件中段读取都正常，取直链的加解密对着真服务器有效。

用得还少，遇到问题请带 `-v` 跑一遍，把日志发到 issue。

协议是照着 115 网页版自己实现的，不依赖任何第三方 115 SDK。取链接口的两层信封实现在 `internal/pan115/crypto.go`，已与一份已知可用的同端点解码器做过逐字节交叉验证，结果固化成了 `crypto_test.go` 里的 golden vector。
