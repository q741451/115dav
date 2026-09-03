# 115dav

把 115 网盘挂成一个**只读** WebDAV，给 Infuse / VidHub / nPlayer 这类播放器在线看片用。

单个 Go 二进制，不依赖任何 115 SDK——协议是照着 115 网页版自己实现的。

```
115dav -cookie 'UID=...; CID=...; SEID=...; KID=...'
```

然后在 Infuse 里添加 WebDAV 来源，地址填 `http://<这台机器的IP>:8115/`。

## 为什么是转发而不是 302

115 发的 CDN 直链跟**取链时用的 User-Agent 和会话绑定**。如果 WebDAV 直接 302 把播放器丢过去，播放器会用它自己的 UA 去取，多半吃 403。

所以这里的 `GET` 是本地转发：工具拿着正确的 UA/Cookie 去 CDN 拉，把播放器的 `Range` 原样透传上去，再把 206 和 `Content-Range` 原样带回来。拖进度条因此是正常的，代价是流量过一遍本机。

## 拿 cookie

浏览器登录 115.com → F12 → Application/存储 → Cookies → `https://115.com`，取 `UID`、`CID`、`SEID`、`KID` 四个值（`KID` 老会话可能没有，没有也行）。

整段粘进去就可以，多余的 cookie 会被忽略：

```
115dav -cookie 'UID=xxx; CID=yyy; SEID=zzz'
```

不想让密钥出现在进程参数里，用文件或环境变量：

```
115dav -cookie-file ~/.115cookie
PAN115_COOKIE='UID=...; CID=...; SEID=...' 115dav
```

启动时会先列一次根目录验证 cookie，不通就直接报错退出，不会等到播放器连上才发现。

## 常用参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `:8115` | 监听地址 |
| `-cookie` / `-cookie-file` | — | 115 cookie，也可用 `PAN115_COOKIE` |
| `-user` / `-pass` | 空 | HTTP Basic 认证，也可用 `PAN115_USER` / `PAN115_PASS` |
| `-root` | `0` | 只挂某个子目录时填它的 115 目录 id |
| `-dir-ttl` | `5m` | 目录列表缓存时长 |
| `-link-ttl` | `2h` | 直链复用时长 |
| `-rate` | `2` | 115 API 每秒请求数上限（不限制读文件） |
| `-page-size` | `1000` | 列目录分页大小，上限 1150 |
| `-v` | 关 | 打印每个请求 |

**默认没有密码**，端口对谁开放谁就能读你的网盘。放在局域网以外一定要加 `-user` / `-pass`。

## 几个实现上的取舍

- **只读是硬的**。`PUT`/`DELETE`/`MKCOL`/`MOVE`/`COPY`/`PROPPATCH`/`LOCK` 一律 405，底层 `webdav.FileSystem` 的写入口也全部返回 `ErrPermission`。整个代码里没有任何一个 115 写接口。
- **直链过期自愈**。`-link-ttl` 只是优化。真正的判据是 CDN 返回 401/403/410——命中就作废缓存重新取链再试一次，播放器看不到中断。
- **并发合并**。播放器拖进度条会瞬间打出一堆 Range 请求；同一个文件的取链、同一个目录的列举都走 singleflight 合并成一次上游调用，否则很容易撞 115 限速。
- **`HEAD` 不走上游**。文件大小从目录列表里就有，`HEAD` 直接本地回答，不浪费一次取链。
- **重名会自动改名**。115 允许同目录同名文件，而 WebDAV 按名字寻址，所以第二个开始改成 `片子 (2).mkv`，保证每个文件都能被访问到。
- **`Content-Type` 自己猜**。CDN 基本一律返回 `application/octet-stream`；这里按扩展名给出 `video/x-matroska` 之类的真实类型，PROPFIND 的 `getcontenttype` 也用它。
- **ETag 用 115 的 SHA1**。比 mtime+size 拼出来的稳定，重启和重新列目录都不会变。
- **超时是分开的**。API 调用 30 秒超时；播放流**没有**写超时，否则长片会被自己掐断。

## 构建

```
git clone https://github.com/q741451/115dav
cd 115dav
go build -o 115dav .
go test ./...
```

需要 Go 1.25+（`go build` 会按需自动下载工具链）。依赖只有 `golang.org/x/{net,sync,time}`。

交叉编译全部平台，和 CI 用的是同一份脚本：

```
VERSION=v0.1.0 scripts/build-all.sh      # 产物在 dist/
```

## CI 与发布

`.github/workflows/build.yml`：

| 触发 | 行为 |
| --- | --- |
| push 任意分支 | gofmt / vet / `go test -race` / 交叉编译十个平台，产物挂在 run 上 |
| push `v*` tag | 同上，然后自动发 Release |
| 网页 Run workflow | 同上，勾选 Publish 并填 tag 才发布 |

### 编译环境是钉死的

产物只应该取决于源码、依赖和工具链，不该取决于哪台机器编的。所以：

- **编译器只在 digest 钉死的容器里跑**（`golang:1.27.1-bookworm@sha256:648f44…`）。钉 digest 不钉 tag —— tag 能被发布者重新指向，digest 就是内容本身。宿主 runner 只负责起进程，它的 OS 怎么更新都碰不到产物。
- **`GOTOOLCHAIN=local`**。这条最容易漏：否则 `go.mod` 里的 go 指令会让 Go 自己下载另一个版本的工具链，容器白钉了。
- **`CGO_ENABLED=0` + 全部交叉编译**。不link系统 libc，不用 macOS/Windows runner，少两套会漂移的环境。
- **依赖由 `go.sum` 按哈希锁定**，CI 里跑 `go mod verify`。

升级 Go 只需要改 workflow 里 `BUILDER:` 那一行。新 digest 这样取：

```
docker buildx imagetools inspect golang:1.28.0-bookworm | head -2
```

### 不是假设，是验证

`verify` job 会在**另一个** runner 镜像（ubuntu-22.04，主构建是 24.04）上用同一个容器重新编一遍，比对二进制的 SHA256。对得上才算数。哪天 GitHub 换镜像真的把构建弄脏了，这个 job 会直接红掉，而不是让 Release 悄悄发出一个不一样的东西。

构建本身是可复现的（`-trimpath` / `-buildvcs=false` / `-buildid=` / 归档时间戳归一），同一个 commit 编两次，二进制和归档都逐字节相同。

### 一个还没做完的点

workflow 里的 action 目前钉到大版本（`actions/checkout@v5` 这种），**没有钉到 commit SHA** —— 写这份代码的环境访问不了 GitHub API，解析不出 SHA，我不想编一个假的进去。要钉死的话在你自己机器上跑：

```
gh api repos/actions/checkout/commits/v5 --jq .sha
gh api repos/actions/upload-artifact/commits/v4 --jq .sha
gh api repos/actions/download-artifact/commits/v4 --jq .sha
```

然后把 `@v5` 换成 `@<sha> # v5`。仓库里的 `.github/dependabot.yml` 已经配好，钉了 SHA 之后 Dependabot 会继续按周提 PR 帮你升级 —— 钉死但不烂掉。

## 协议实现说明

取链接口 `POST proapi.115.com/app/chrome/downurl` 的请求体和响应体都套了两层信封（一层自定义 XOR + 一层 RSA），必须逐字节一致才会被服务端接受，实现在 `internal/pan115/crypto.go`。

该实现已与一份已知可用的同端点解码器做过逐字节交叉验证（响应方向在随机输入上输出完全一致，两张常量表逐字节比对），验证通过后固化成了 `crypto_test.go` 里的 golden vector，因此仓库本身不依赖那份参考实现。

## 状态

- 加解密、目录解析与分页、WebDAV 与 Range 转发：有测试覆盖（含 `-race`），其中列目录用按真实 JSON 形状构造的 stub 压过，转发链路用假 CDN 压过（含直链过期重取）。
- **未经真实 115 账号验证**：写这份代码的环境里没有可用 cookie，所以对接真实 115 服务器的那一跳还没跑过。第一次用请带 `-v` 跑，有问题把日志贴出来。
