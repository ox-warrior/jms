# jumpserver-cli

两个二进制：`jms`（用户笔记本上的 TUI/CLI 客户端）和 `jmsd`（机房里的守护进程，持有 Core 的 API 凭据和只读库账号）。

## 构建和发版走 Makefile

仓库里只有 `Makefile.example`（占位符地址），本地的 `Makefile` 是从它拷出来
改真实桶名的，已 gitignore。没有 `Makefile` 就先 `cp Makefile.example Makefile`。

```bash
make check                        # gofmt + vet + test，提交前跑
make build                        # 本机三个二进制
make jmsd-release VERSION=0.2.0   # 机房那台的 jmsd（linux/amd64 静态链接）
make release      VERSION=0.2.0   # jms 三平台 + 上 CDN（会覆盖生产）
make release-dry  VERSION=0.2.0   # 同上但只编到 dist/，不碰网络
```

**别再手写 `go build`。** `internal/version.Version` 是 ldflags 注入的，源码里
躺的是 `dev`，而 `/v1/whoami` 报的就是这个值、客户端拿它跟自己比——手编一个
`dev` 的 jmsd 会让每个人的界面顶上都挂一条「版本不一致」，还指向错的方向。
三个 release 目标在缺 `VERSION` 时直接失败，就是为了堵这个。

**发版顺序是 jmsd 先、jms 后。** 云元数据、`cloud_key` 和 `OWNER`/
`INSTANCE TYPE`/`MODULE` 三列全靠 jmsd 下发；反过来的话先更新的用户会拿新 jms
连老 jmsd，`d` 面板全说「没有元数据」、三列全空。

**jmsd 不进 CDN**，那套自动更新只服务用户笔记本上的 jms。

## 跑测试用 sub-agent

**不要在主对话里跑 `go test ./...`。** 派一个 sub-agent 去跑，让它只回报失败，通过的包不要逐行贴回来。

```
Agent(subagent_type: "general-purpose", model: "haiku", prompt:
  "在 <repo> 跑 `go test ./... -count=1`。只回报：整体通过/失败，以及每个失败的
   包名、测试名、失败输出。全通过就回一句 PASS，不要贴通过的包列表。")
```

理由是 token，不是速度：一次全量输出会把十几个包的 `ok` 行和 cgo 警告全灌进上下文，而这些信息量是零。`gofmt -l` / `go build` / `go vet` 很安静，可以直接在主对话跑。

同理，**不要试图去数测试数量**。仓库外挂了 RTK 代理会过滤 `go test -v` 的输出，`grep -c '=== RUN'` 的结果不可信；要数就 `grep -rh '^func Test' internal/ --include='*_test.go' | wc -l`。

## 改动约束

- **不要影响 TUI 的功能。** 客户端的价值全在那个界面上，任何重构都要保证 `internal/tui` 的测试原样通过。
- **`jms` 和 `jmsd` 是两套东西。** `internal/jmsd/` 里绝大部分是守护进程的实现，只有 `kind.go` 是两边共用的资源分类表。改这个目录前先确认改的是哪一边（`go list -deps ./cmd/jmsd` 会告诉你守护进程实际链了什么）。
- **票据、kubeconfig、TOTP 码不许落到日志、剪贴板或磁盘。** 只有 `ticket.ID` 可以进日志，`ticket.Value` 不行；kubeconfig 只允许待在 `k8sclient.WriteTemp` 自己那个私有临时目录里。
- **k9s 的存在性必须在签票之前检查**（`connectflow.PrepareK8s` 里已经把这个顺序焊死了）。签票之后再失败会白占一张票和一条 Core 审计记录。

## 测试的取舍

现有测试绝大多数是**回归测试**——每一个都对应一次真实发生过的 bug（焦点跑到左树、MFA 弹框死循环、空过滤结果按方向键卡死、子树计数重复、搜索被左树光标限制住）。删这类测试等于丢掉那段历史。

该删的是另一类：断言 switch 语句自己、遍历 map 的 key 再去那个 map 里查、或者验证标准库/tcell 行为的测试。判断标准是「这个断言失败时，是不是真有用户会遇到问题」。

## 注释

仓库基线是注释/代码约 35%。值得写的是：安全不变量、顺序约束、并发和生命周期、以及「为什么不是那个显而易见的写法」。不值得写的是把下一行代码用中文再说一遍。

## 文档分工

`README.md` 是用户怎么用（参数、快捷键、配置、部署步骤），也是公开仓库里唯一的文档。
设计决策和取舍写在源码注释里，跟着代码走。
