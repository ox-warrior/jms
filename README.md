# jumpserver-cli

`jms` 是 JumpServer 的 Go 客户端，提供 Koko 的 K9s 风格终端 UI。已在 JumpServer / Koko `v4.10.16` 上验证。

> **关于 `docs/`**：源码注释和下文多处引用 `docs/PLAN.md` / `docs/K9S_LOCAL_PROXY_PLAN.md`
> 的章节号。这两份是内部设计取证文档，不在公开仓库里；引用保留是因为它们同时也是
> 内部代码审查的索引。README 本身是自足的，不看那两份也够用。

## 架构设计

本仓库产出两个二进制：`jms`（用户本机运行的 TUI 客户端）和 `jmsd`（内网控制服务，`docs/PLAN.md` 是完整的方案取证文档，本节是它的摘要）。

### 整体架构

```mermaid
flowchart LR
    subgraph client["用户本机"]
        key["~/.ssh/id_ed25519\n(本地私钥，只签名不上传)"]
        jms["jms\n(TUI 客户端)"]
        k9s["K9s\n(K8s 资产)"]
    end

    subgraph ctrl["内网控制面"]
        jmsd["jmsd\n(控制服务)"]
        redis[("Redis\nMFA 状态 / nonce，可选")]
    end

    subgraph js["JumpServer"]
        core["Core\n(REST API)"]
        pg[("PostgreSQL\n(jmsd 只读账号)")]
        koko["Koko\n(会话网关 :5002)"]
    end

    target["目标资产\n服务器 / 数据库 / K8s"]

    key -.签名 fp·ts·nonce·sig.-> jms
    jms -- "① 身份反查 · MFA · 查资产\nHTTP + SSH 签名" --> jmsd
    jmsd -- "只读 SQL：验签 / TOTP" --> pg
    jmsd -. "MFA 时间戳 / 防重放 nonce" .-> redis
    jmsd -- "② Access Key 签名\n权限树 / 出票" --> core
    core --- pg
    jms -- "③ ssh JMS-<票号> / <票值>" --> koko
    koko -- "④ Account.Secret（jms 不可见）" --> target
    jms -. "K8s 资产直接拉起本机 K9s" .-> k9s
    k9s -- "kubectl，经 Koko 反代" --> koko
```

终端流量（shell / 数据库客户端 / kubectl）由 `jms` 直连 Koko，不经过 `jmsd`；`jmsd` 只负责鉴权、查权限、出票这三件事。

### 设计原理

起点是三条硬约束，合起来唯一确定了这个架构：

```
不输密码 ⟹ 身份只能靠 SSH 私钥证明
        ⟹ 验证公钥需要 JumpServer 侧的高权限凭据
        ⟹ 这个凭据不能下发到用户电脑
        ⟹ 必须有一个独立的服务端持有它（jmsd）
```

认证链分四段，每段用不同的凭据，权限逐段收窄：

| # | 谁到谁 | 认证方式 | 目标资产密码是否可见 |
|---|---|---|---|
| ① | `jms` → `jmsd` | 本地 SSH 私钥对 `fingerprint\|ts\|nonce` 签名 | 否 |
| ② | `jmsd` → JumpServer Core | `jmsd` 服务账号的 Access Key 签名（`SigAuth`） | 否 |
| ③ | `jms` → Koko | 用户名 `JMS-<票号>`，密码 `<票值>`，程序自动填 | 否（票据不含 `Account.Secret`） |
| ④ | Koko → 目标资产 | 票据内的 `Account.Secret` | Koko 持有，`jms`/`jmsd` 均不参与 |

几个关键取舍：

- **身份 = SSH 公钥指纹，不是用户名/密码**：`jms` 本地用私钥签名，`jmsd` 反查这把公钥登记在哪个用户名下，省掉登录页和一切输入框。
- **验签在数据库层做，不走 REST API**：JumpServer 的公钥登录后端要求 service account 签名，而 service account 这条路全网无文档、官方 SDK 和 Koko 自己都不用（`docs/PLAN.md` §5.B）。`jmsd` 改用「只读 DB 账号 + Core 的 `SECRET_KEY`」直接读取并本地解密 `authentication_sshkey`/`otp_secret_key`，绕开这个被堵死的入口。
- **票据制，密码不出 Core**：`jmsd` 用最小权限服务账号换一张 `ConnectTokenInfo`（只有 `{ID, Value, ExpireTime, AccountName, Protocol}`），`jms` 拿票号/票值当 SSH 用户名/密码连 Koko；目标资产的真实密码只在 Core→Koko 之间流动，两个自研组件都碰不到。
- **无 session，每请求现场签名**：不引入一个可被窃取的会话令牌，改成每次请求带 `{fp, ts, nonce, sig}`，30 秒窗口防重放。
- **资产全量拉取 + 全部在客户端过滤**：`/v1/assets` 不接受节点过滤参数，一次性返回该用户全部有权限的资产；分类、`/` 搜索、左侧树的每节点计数都在本地对同一份数据算——按节点分别查询会让其他节点的计数归零，这是已经踩过的坑。
- **资产缓存 12 小时，鉴权永不缓存**：资产列表和节点树缓存在本地文件里换取秒开；但 `/v1/whoami`（身份+MFA 新鲜度）和 `/v1/connections`（出票）永远实时请求。
- **公钥指纹索引缓存 30 秒**：`authentication_sshkey.public_key` 用随机 nonce 的 AES-256-GCM 加密，同一把公钥每次密文都不同，建不了索引，只能整表扫描逐把解密。不缓存的话，一个签名乱填的伪造请求就能让 `jmsd` 对 Core 的生产库跑一次全表解密；缓存后伪造请求只值一次内存查找。

### 核心功能

- 零输入登录：本地 SSH 私钥指纹反查身份，不用输用户名/密码
- MFA 周期由 `jmsd` 自己控制（默认 10 天），与 JumpServer 全局 MFA 策略解耦；连续输错 5 次验证码锁定 15 分钟
- K9s 风格 TUI：`:` 命令输入、`/` 搜索、按云资源分类切换（`:all`/`:ec2`/`:rds`/`:redis`/`:k8s`），两者可叠加且持续生效
- 收藏（`y` 键）与最近连接，直接读写 JumpServer 后端数据，和网页控制台双向同步
- 云主机 Describe（`d` 键）：机型、CPU/内存、磁盘、安全组、VPC、计费方式、标签，来自 cloud-sync 落库的云元数据
- 本地资产缓存（默认 12 小时 TTL，`r` 强制刷新），按「服务器+用户名」分文件
- K8s 资产始终在本机拉起 K9s，通过 Koko 反向代理连接，不提供远程 shell 或自动回退
- 支持 Koko 已支持的协议家族（ssh/telnet/sftp/各类数据库等），`jmsd` 出票前会做出口检查并过滤掉 `@INPUT` 类账号
- `jms`/`jmsd` 构建版本、双方各自声明的 JumpServer 基线，任一不一致都会在界面顶部提示
- 三套内置主题（`k9s`/`light`/`mono`），响应式列宽保证窄终端下资产分类列不被挤掉

完整的、逐条带背景说明的功能变更记录见下方「功能范围」。

### 用到的 JumpServer 特性

| JumpServer / Koko 组件 | 用途 |
|---|---|
| `authentication_sshkey` 表（`AES-256-GCM` 加密，随机 nonce） | `jmsd` 直读并本地解密，做 SSH 公钥指纹反查与逐把验签 |
| `users_user.otp_secret_key` | `jmsd` 本地校验 TOTP（`pyotp` 同参数：SHA1/6 位/30 秒），实现自控 MFA 间隔 |
| Core 的 Django `SECRET_KEY` | 派生 AES-256-GCM 密钥，解密上面两个加密字段 |
| `perms/users/{id}/nodes`、`perms/users/{id}/assets`（sdk-go `GetUserNodes`/`GetAllUserPermsAssets`） | 拉取用户权限树与全部有权限的资产（含 `nodes` 归属，用于左树计数） |
| `authentication.add_superconnectiontoken`（sdk-go `CreateSuperConnectToken`） | `jmsd` 出票，返回不含密码的 `ConnectTokenInfo` |
| RBAC 自定义最小权限角色 | `jmsd` 服务账号只拿 `perms.view_userassets` + `authentication.add_superconnectiontoken`，明确不给 `authentication.view_superconnectiontokensecret` |
| Access Key（普通账号的个人 API 密钥，非 `is_service_account`） | hop② 的 `SigAuth` 签名鉴权；service account 因未文档化、无维护而被弃用 |
| Koko `JMS-<票号>` SSH 登录 | `jms` 用票号做用户名、票值做密码连 Koko，短路 Koko 自身的 MFA 挑战 |
| `assets_favoriteasset` 表 | 收藏读写；REST 收藏接口按调用者身份返回，拿不到「某用户收藏了什么」，只能走表 |
| `terminal_session` 表 | 「最近连接」数据源，同时覆盖网页端和 Koko 直连产生的会话，不局限于 `jms` |
| Koko `connect_methods`（ssh/telnet/sftp/mysql/postgresql/redis/mongodb/k8s 等） | 决定 `jms` 能连接的协议范围；`db2`/`dameng` 不支持，仅 Chen（Web GUI）可接 |
| K9s + Koko `:5002` 反向代理 | K8s 资产的连接方式：`jms` 生成临时 kubeconfig，本机 K9s 经代理连 Koko |

## 功能范围

- 服务器地址和 jmsd 地址都可以写进配置文件，不用每次带参数；命令行参数优先，见下方「客户端配置」
- **不用输用户名**：启动即用本地 SSH 私钥的公钥指纹向 jmsd 反查身份，见下
- SSH 私钥按顺序查找：显式指定 → `~/.ssh/id_rsa` → `~/.ssh/id_ed25519`，不读 `ssh-agent`
- 启动不会直接连 Koko——只在真正选中资产连接、或 jmsd 要求验 MFA 时才会跟 JumpServer 打交道，见下
- `:q`（`:quit` / `:exit` 同义）退出，和 `Ctrl+C` 完全等价——走的就是 tview 事件循环里 `Ctrl+C` 调的那个 `Stop()`，日志照常收尾。`:` 本来就是这个客户端的命令入口，从 vim 或 k9s 过来的人第一反应就是 `:q`。打错的词（`:qq`、`:q!`）什么都不做，不会把会话关掉
- 已实现 `:all`（默认，不过滤）、`:ec2`（含 AWS EC2 与阿里云 ECS）、`:rds`、`:redis`、`:k8s` 按云资源分类切换、`/` 过滤、按 Enter 选中资产直连（见下）；这几个分类之前是空实现——切哪个看到的都是同一批资产——本轮才第一次真正按 JumpServer 的 `category`/`type` 过滤
- 资源表最后一列是 `SOURCE TYPE`，标出每行是 `ec2` / `rds` / `redis` / `k8s`——在 `:all` 视图下这是唯一能一眼看出「这是台机器还是个库」的地方，之前得靠名字猜。这一列和 `:` 分类用的是同一套判定，所以标成 `redis` 的行一定就是 `:redis` 会列出来的行。四类之外的资产（比如 `web`）直接显示 JumpServer 的 category，不显示占位横杠——横杠会被读成「数据缺失」，而 category 本来就说明了它是什么
- **默认打开的是 `:ec2` 不是 `:all`**：机器是大家来这儿要找的东西，而且只有这个视图有那几列按类型的信息——开在这儿等于把它们摆在面前，而不是藏在一个得先知道的命令后面。`:all` 一个键的事
- **`:host` 和 `:ec2` 等价**：左树里那些节点标的就是 `host`（JumpServer 自己的 category 名），看见了自然会去敲。**以前敲 `:host` 什么都不会发生**——命令分发自己抄了一份类型词表，那份里没有 `host`，而且没有 default 分支，所以屏幕原样不动。「切换失败了」和「没这个命令」从外面看一模一样，而后者才是答案。现在类型词表只有一份（`jmsd.ParseResourceKind`），认不出来的命令会在状态栏说清楚并列出可用的
- **资源表的列跟着资源类型走**：`:ec2` 下是 `NAME · OWNER · ADDRESS · INSTANCE TYPE · MODULE · SOURCE TYPE`，`:all` / `:rds` / `:redis` / `:k8s` 下只有 `NAME · ADDRESS · SOURCE TYPE`。这几类资产的数据本来就不一样——机器有机型，数据库有引擎和版本，K8s 集群两个都没有；一套固定列要么显示一列对多数行是空的，要么就漏掉真正能区分它们的那个字段。加新列是加一条 `extraColumns` 记录 + jmsd 那边一个字段，**后者才是贵的**：那些字节每次列表、每个资产都要付，所以门槛是「扫列表的时候值得看见」，其余的全部留在按 `d` 后面
  - 这几列的宽度是**预留**的，和 `SOURCE TYPE` 一样从 NAME/ADDRESS 的预算里扣。不扣的话缩窗口第一个消失的就是它们（tview 从右往左丢），而且表现是标题被截断而不是整列没了，更难发现
  - 预留值取的是**生产数据里最长的那个**，不是拍的：机型 18 字符、团队 12（`Payments`）、模块 7（`Monitor`）
  - **窗口窄到放不下时，按类型的那几列被压窄（截断成 `…`），不是被丢掉，也永远不动 `SOURCE TYPE`**。tview 从左往右排、放不下的直接丢，所以真会消失的是最右边那列——也就是 `SOURCE TYPE`，而那正是仓库里一条真实 bug 的回归测试盯着的东西
  - 预算的花法就是全部策略，按「没了就没法用」排序：① 每列一格的分隔符 ② `SOURCE TYPE` 的预留、`NAME`/`ADDRESS` 的下限（这三列每行都有内容）③ 按类型的列从左往右各拿自己的预留，不够就压到剩下多少、最少一格（一个 `…` 至少还留着列和标题，直接丢掉会让人怀疑这个字段存不存在）④ 还有余量再回去加宽 NAME/ADDRESS
  - **列的集合和窗口宽度无关**。曾经写成「放不下就丢列」，那是错两次：单元格是在知道宽度之前写好的，改集合意味着要在表格自己的 draw 函数里重建它；而唯一看起来安全的延后手段 `QueueUpdate` 是**阻塞等事件循环执行**的——在 draw 里调用就是事件循环在等自己
  - `MODULE` 空着是正常的（生产上 72% 的机器有这个标签），所以空就是空、**不打横杠**——一个平时有值的列里出现横杠，会被读成数据没加载出来
- **缩窗口时 `SOURCE TYPE` 不会被挤掉**：tview 从左往右分配列宽、放不下的列直接丢弃，而这一列在最右边——不做处理的话，一个 50 字符的 RDS endpoint 就能把表吃光，缩窗口第一个消失的恰好是这一列。所以 NAME 和 ADDRESS 都带宽度上限，按需生效：先让 ADDRESS 收（它最长、后缀高度重复，也是挑行时最不需要读的），ADDRESS 到 16 字符下限之后再让 NAME 收到 12。60 列的终端里 `SOURCE TYPE` 仍然完整。宽终端上不加任何上限，不会无谓截断
- **启动时左树和资产是同一帧出现的，不再闪一下**：以前是两次独立的界面更新——先装左树，而那一刻资产还没到，所以每个节点的匹配数都是 0、整棵树按「什么都没匹配上」渲染了一帧，第二帧资产落地才变成真的。左树的计数本来就是从资产算出来的，隔一帧画必然对不上
- **`/` 搜索覆盖当前视图的每一列**，不只是名字和地址：`:ec2` 下搜 `kafka`（MODULE）、`payments`（OWNER）、`m5.2xl`（机型）都能搜到——那几列摆在那儿就是给人扫的，不能搜等于白摆。搜不到的是**当前视图没显示的列**：`:all` 下没有 OWNER 列，就不会因为归属团队匹配而蹦出一行，否则屏幕上找不到任何理由解释它为什么在这儿，和过滤器坏了没法区分
  - **和窗口宽度无关**：窄窗口会丢掉放不下的列，但搜索照旧覆盖那个类型声明的全部列——「拉一下窗口匹配数就变了」是更没法解释的行为
- **分类和 `/` 搜索是两个独立、可叠加的过滤条件，且都会一直生效**，直到你自己清掉：切左侧节点、切分类、后台刷新都不会把搜索条件冲掉（这之前是个 bug——切节点后资产重新加载，搜索被静默丢弃，看到的是该节点下的全部资源）。当前搜索条件会显示在底部状态栏（`/web (3/128)`，括号里是匹配数/总数），再按一次 `/` 会带着原来的关键词重新打开输入框，方便继续收窄而不用重打。浏览态（非命令输入、非弹框）按 `Esc` 一次性清掉分类和搜索，回到 `:all` 全量
- 左侧 Scope 树跟着当前分类和 `/` 搜索走：子树里一个匹配资源都没有的节点直接从树上消失，剩下的节点名后面标匹配数（`ack (4)`）。计数完全在客户端用已加载的资产算，**不会为此多发任何请求**——每个资产本来就带着自己的节点归属。当前选中的节点即使变空也会保留、置灰，这样切分类不会把光标扯走
- **本地缓存**：首次拉到的资产列表和节点树会在本地存一份，带时间戳。下次启动如果没超过 12 小时就直接用缓存，**一个网络请求都不发，秒开**；超过 12 小时才重新拉。按 `r` 永远绕过缓存、强制重读。用 `--cache-ttl` 调整（如 `--cache-ttl 1h`），`--cache-ttl 0` 关掉
- 缓存放在 `<用户缓存目录>/jms/`（macOS `~/Library/Caches/jms/`，Linux `~/.cache/jms/`），权限 `0600`，**按「服务器 + 用户名」分文件**——同一台机器上两个人的可见资产不一样，绝不能互相串。文件里存的是资产名和地址，删掉它只会让下次启动慢一点。收藏和最近**不缓存**：它们在 Core 里、只要一次请求，而「最近」过期 12 小时比慢一点更糟
- 缓存不影响 MFA：启动时的 `/v1/whoami` 从不走缓存，10 天的 MFA 检查照常在启动时触发；真正连机器时的 `/v1/connections` 也永远是实时鉴权
- **资产只在启动和按 `r` 时整体拉一次，之后节点、分类、搜索全是本地过滤**，所以在树上移动光标是瞬时的、不产生任何网络请求。这一条是必须的：树上的计数要在整棵树的范围内统计才有导航价值，而按节点去拉子集会让**其他节点的计数全部归零**——之前就是这个 bug，光标一移到某个叶子上，兄弟节点直接从树上消失、根节点计数也跟着缩水
- **收藏与最近**：左侧三个虚拟入口（★ Favorites / ◷ Recent / ● All permitted）之前是空占位——点哪个都显示全部资产——现在真的能用了。在资源表上按 `y` 收藏/取消收藏当前行，收藏的行名字前面会带 `★`。三个入口都带匹配数，即使是 0 也保留显示（空的 Favorites 正是用户发现「有收藏这功能」的地方），并且都能和分类、`/` 搜索叠加——站在 Recent 里切 `:rds`，就是「最近连过的数据库」
- **收藏和最近都存在 JumpServer 里，本地不落任何文件**：所以换台机器跑 `jms`，收藏跟着走；在网页控制台点的收藏，`jms` 里也看得到，反之亦然。`◷ Recent` 直接来自 Core 的会话历史，因此**从网页端或直连 Koko 连的机器也会算进去**，不只是 `jms` 连的；只统计成功建立的会话，同一台机器取最近一次、不重复列出
- 按 `y` 时星标**立刻出现**，不等服务端返回；万一写入失败会自动撤销并在状态栏报错，不会让界面显示一个其实没存上的星标
- **`/` 搜的是全部有权限的资产，不受左树光标位置限制**：左树的光标一移动就会把范围缩到那个节点（它绑的是「光标变化」而不是「按回车确认」），所以在树上翻几下再按 `/`，搜索其实只覆盖了你恰好停在的那个子树——而要找的机器多半在别的节点下，看起来就是「搜不到」。现在 `/` 一律跨全部资产搜，「没有匹配」就真的等于没这台机器。**搜索期间左树光标会移到 `● All permitted`**，因为那就是右表实际在显示的范围——光标要是还停在某个节点上，等于树上写着一个范围、表里列着另一个范围的结果。左树的每节点匹配数照常按搜索词统计，所以它继续告诉你**搜到的东西在哪个节点下**；清掉搜索（或按 `Esc`）光标自动回到你原来站的那个节点，搜索是绕个路，不是丢掉位置。`★ Favorites` / `◷ Recent` 里搜仍然只搜那里面——那两个是你主动点进去的。**但搜索期间点左树的节点是有效的**：树上的每节点匹配数就是在告诉你「搜到的东西在哪」，顺着它点过去却毫无反应，整个左栏读起来就是死的（这是个 bug，已修）。现在搜索期间选中某个节点，右表立刻收进「那个子树 ∩ 搜索词」，光标也留在那个节点上、不再被每敲一个字就拽回 `● All permitted`；清掉搜索之后你留在**点过去的那个节点**，而不是开始搜索前站的那个（后者更早、也更不像是你的本意）。「搜索默认跨全部资产」这条没变——路过某个节点不等于选了它，只有搜索开始之后的那次选中才算
- **搜完之后光标落在搜索结果上**：`/` 在左树和右表里按都能开(它绑在整个工作区上,不挑面板),但之前从左树按 `/` 搜完、回车之后焦点回到左树 —— 按 ↑↓ 动的是节点,不是刚搜出来的机器,得先 Tab 一下才碰得到结果。搜的是资源,搜完就该在资源上,所以现在回车一律落到右表第一个匹配行,直接按 Enter 就能连。按 `Esc` 取消不算搜索,原来在哪还回哪
- **`:` 和 `/` 打开的是一个带边框的输入框**（参考 k9s），底部那行提示条整个被它替掉，不是在提示条旁边挤一个输入格。两个提示符故意不一样：`:` 是 `⚡ >`，`/` 是 `🔍 >`——框一打开，你按的那个 `:` 或 `/` 就从屏幕上消失了，提示符是唯一还能一眼回答「我这是在过滤还是在敲命令」的东西。**两个框的边框上都写着 `Enter` 和 `Esc`**——一个看不出怎么退出的输入框是最难受的，而 `:` 之前把整条边框都用来列命令了，压根没提 Esc。命令列表挪进输入框当占位文字（`⚡ > all · ec2 · rds · redis · k8s · doctor · q`），一打字就自动让开。`Esc` 在两个框里都是取消：框关掉、焦点回到表格、不留下任何过滤条件。**用鼠标点走焦点也会把框关掉**：点搜索结果里的某一行等同于按 `Enter`（过滤条件保留、落在你点的那行），点其他地方时 `:` 里没跑的命令直接作废。这之前是个 bug——tview 的表格是在自己的鼠标处理里抢焦点的，不经过按键那条路，所以框还开着、程序还以为你在打字，而键盘已经归表格了：`Esc`、`y`、`r`、`Tab` 全部按了没反应，只有 `↑↓` 还能动，唯一的出路是用鼠标点回那个框里
- **收藏永远是小写 `y`，只有这一个键**：`/` 是边打边过滤的，之前搜索框吃掉了所有按键，光标还卡在第一行，所以「打几个字找到它、然后收藏它」这条路走不通。现在搜索框里 `↑↓` 直接移动下面表格的选中行，回车关掉输入框（过滤条件保留），落在你刚选中的那一行，按 `y` 收藏。搜索框里不绑任何字母：一个字母没法同时是文本和命令，绑了 `y` 就再也搜不了 `mysql`、`proxy`、`yum`。所以唯一的限制是「正在打字的那几秒按不了」，回车之后照旧
- **按 `d` 看这台机器在云上是什么**：JumpServer 只存名字和地址，机型、内存、磁盘、安全组、是不是包年包月它一概不知道——想知道就得开控制台、还得先想清楚这台在哪个云、哪个账号下。这些数据由姊妹项目 cloud-sync（jumpserver-tool）每 30 分钟同步一次到一张 MySQL 表，`jmsd` 只读
  - **资产列表里只带一个短 key，按 `d` 才去取那一条**。全量元数据几百 KB，绝大多数行没人会去看，全塞进每次列表是为了省一个可能永远不发的请求而花掉几百 KB。jmsd 侧那一条是内存 map 查找，不碰数据库
  - **列表里没有 key 的行，按 `d` 一个请求都不发**——列表已经说了这行没东西可看，再跑一趟只为被告知「没有」，用户是能感觉到的
  - 找得到对应云资源，靠的是 cloud-sync 写在资产 comment 里的 `[cloud-sync:{云}:{账号}:{实例ID}]`——**手工登记的资产按 `d` 会明说「不是 cloud-sync 管的」**，而不是弹一个空面板；**RDS / Redis 会说「元数据不在这张表里」**，它们也被同步成了资产，但元数据在各自的表里，这张表只有主机
  - **包年包月的机器会写清楚有没有开自动续费**：只给一个到期日期，看不出那是死线还是会自动往后挪，而这两件事要做的处理完全相反。「未开自动续费」和「查不到」是两种显示——把查不到当成没开，会让人半夜跑去控制台看一台其实一直在自动续的机器
  - AWS 拿不到「包年包月」这个概念，所以那一栏在 AWS 机器上**整行不显示**，而不是显示成「按量付费」——后者是一句听着很确定的假话。同理拿不到的 CPU/内存不会显示成 0
  - **按 `d` 推开的是一整屏的视图，不是浮框**（照 k9s 的 describe 来的）。这里的内容是一份文档——四十来行两列文本，标签多了更长——浮框既装不下也读起来像被打断，而用户想做的只是看完然后回去
  - **重复的值只写一次标签**：一台机器挂四个安全组，以前会把「安全组」这个词打四遍——那个词不是行与行之间的差别所在，重复它是把注意力花在标签上而不是值上。现在标签只出现在第一行，后面几行标签列留空、值对齐
  - 视图里 `/` 搜索，**按行过滤**并在底部显示 `3/42 行`；`Esc` 先清过滤条件、再按一次才返回，所以「取消这次narrowing」和「离开」不会被同一下按键一起做掉。`y` 复制**当前看到的内容**——正在过滤时复制全文，等于给了一份你没在看的东西
  - `q` 关闭，`↑↓`/`j`/`k` 滚动、`g`/`G` 跳首尾、`Ctrl+F`/`Ctrl+B` 翻页（vi/less 的绑定，不是「查找」——这界面本来就说 vi），**鼠标能直接框选**
  - **`Esc` 退回来之后，光标还在你按 `d` 之前那一栏**：切页会把焦点交给页面的默认子元素，而资源页的默认子元素永远是左侧 Scope 树，它的 `SetFocusFunc` 会顺手把「当前在哪一栏」写成「树」——所以从右表按 `d`、退出来会落到左边。取焦点快照要在切页**之前**取
  - **标签的 key 原样显示，不做任何缩短**：前缀往往就是意思本身，`ecs: on` 什么都没说，`Monitor:ecs: on` 才说清楚。一个粘回控制台搜不到的 key 也没什么用
  - **但 `aws:` / `acs:` 前缀的标签整类不显示**：那两个前缀是两家云保留给平台自己用的——启动模板 id、CloudFormation 栈 id、发起 API 调用的账号。生产机器上这些占标签总数三分之一，没有一个在描述这台机器。用前缀规则而不是列具体 key：后者会在每次有人用了新的云功能之后需要修，而且总是修得太晚——视图打开期间 jms 主动把鼠标交还给终端，关掉再收回来。否则 TUI 的鼠标支持（滚轮、双击复制）会把终端原生选区顶掉，只能靠按 Shift 绕，而那是个得有人告诉你才知道的事
  - **元数据库挂了只会让面板变空，不会影响资产列表**：那张表跟登录、鉴权、连接都没关系，为它把整个工具搞不可用是笔烂账
- 选中资产按 Enter：账号/协议只有一个自动选，协议里有 `ssh` 时默认选 `ssh`（不弹框），否则多个才弹选择框；K8s 资产始终在用户电脑运行 K9s，不提供远程 shell 或自动回退
- **双击复制到剪贴板，按列决定复制什么**：双击 NAME 复制机器名，双击 ADDRESS 复制地址，双击 SOURCE TYPE 也是复制机器名（复制个 `ec2` 没意义）。主要是给 `jms -c "ec2 <名字>"` 用的：那条命令是精确匹配，名字必须一字不差。复制的是**底层数据里的完整值**，不是屏幕上那串——NAME 和 ADDRESS 在窄终端下都会被截断成 `prod-web-ap…`，鼠标框选只能拿到截断后的，拿去连必然失败。双击不会触发连接（连接只认 Enter），滚轮和单击选行也完全不受影响。成功会在状态栏用紫色回显复制了什么，失败用红色如实报错，不会假装成功。**仅 macOS**（走 `pbcopy`）：Linux 的剪贴板需要 `xclip`/`wl-copy` 这类不一定装了的外部程序，与其三条路径三种失败方式，不如只做一条永远对的
- **退出终端回到界面时，光标还在原来那一栏**：tview 关掉一个弹层后会把焦点交给下面那页的默认子元素，而资源页的默认子元素永远是左侧 Scope 树——所以关掉任何弹框（账号选择、协议选择、MFA 输入、错误框）都会把光标扔到左边。这个现象会延迟暴露：弹框关掉时会话马上就开始了，等你退出终端回到界面才看见光标跑了，所以看起来像是「退出终端导致的」。现在所有弹层的关闭都走同一个入口，关掉之前先记下你在哪一栏、关掉之后放回去（只在焦点确实掉进资源页时才放，别的弹层还开着就不抢）

## 安装

macOS（arm64 / x86_64）和 Linux（x86_64）都支持，自动识别：

```bash
curl -fsSL "$JMS_CDN_BASE/install.sh" | bash
```

`$JMS_CDN_BASE` 是你们自己的发布地址（`scripts/release.sh` 上传到的那个）。本仓库不内置任何发布源，源码里的默认值是 `https://dist.example.com/jms`——上传时 `release.sh` 会把真实地址替换进 CDN 上那一份 `install.sh`，所以从 CDN curl 下来的那份不需要设环境变量，直接跑就行。

装到 `/usr/local/bin/jms`（没有写权限就用 `sudo bash` 跑上面这条）。Linux arm64 暂未提供预编译二进制，需自行从源码构建，见下。

### 自动更新

装完之后不用再手工跑那条 curl。`jms` 每次启动会问一下 CDN 有没有新版，有就在 TUI 起来之前提示一次：

```
jms 0.1.3 可用（当前 0.1.2）
  [Enter] 更新并重启    [s] 跳过该版本    [n] 这次不用
```

- **Enter** —— 下载、校验 sha256、原子替换掉当前这个二进制，然后带着原来的命令行参数重启。整个过程通常 20 毫秒以内，落到的还是你本来要去的界面
- **s** —— 跳过**这一个版本**，记在 `~/Library/Caches/jms/update.json`（Linux `~/.cache/jms/update.json`）。下个版本发出来还是会问
- **n** 或其他任何键 —— 这次不管，下次启动再问

几件不会发生的事：

- **`-c "type name"` 那条快捷路径永远不提示。** 那是给脚本用的，问一句就等于挂在那里。stdin 不是终端时（管道、cron、CI）同理，一律跳过
- **不会自动装。** 没按 Enter 就什么都不动，也没有后台静默替换
- **不会盖掉源码构建。** 版本号是逐段比大小而不是判等，本地版本高于或等于 CDN 就完全安静——所以发版前把 `internal/version/version.go` 的版本号 bump 上去，整个开发窗口里自己机器上的构建都不会被线上的旧版盖掉
- **`/usr/local/bin` 不可写时不会去提权。** 它会打一行手工更新的 curl 命令，然后正常启动

网络不通、CDN 挂了、平台没有对应的发布版本——全部静默跳过，启动最多为此等 2 秒。想看为什么没提示，把日志级别调成 `debug`。彻底关掉：配置文件里写 `disable_update_check: true`。

### 发布新版本（维护者用）

`scripts/release.sh` 交叉编译全部已发布架构并上传到 S3，`install.sh` 就是从这里下载的。发布位置走环境变量，仓库里不写死：

```bash
export JMS_S3_BUCKET="s3://your-bucket/jms"
export JMS_CDN_BASE="https://your-cdn.example.com/jms"

scripts/release.sh 1.2.3              # 打印目标和版本号，交互确认后编译+上传
scripts/release.sh 1.2.3 -y           # 跳过确认，CI 里用
scripts/release.sh 1.2.3 --build-only # 只编译到 dist/，不接触 S3，本地验证用
```

**版本号是必填参数**，通过 `-ldflags` 注入 `internal/version.Version`，源码里不再有手改的常量（那个常量曾经是「发版忘了改一行 → 二进制自称旧版本 → 更新提示要么不弹要么装完还一直弹」的来源）。脚本会校验它是 `X.Y.Z` 形式：客户端按 semver 比大小，带 `v` 前缀或 `-rc` 后缀的解析不了，会被当成「读不懂，那就不提示」静默掉。目标平台列表是脚本里的 `TARGETS` 数组，新增一个架构只需要加一行 `"GOOS GOARCH s3目录名"`；目录名沿用 `mfmc` 的老命名（`darwin`=macOS arm64，`amd`=macOS x86_64，`linux`=Linux x86_64——历史遗留，新增时跟着这个习惯就行）。上传完会逐个 `curl` 一遍 CDN 确认 HTTP 200，不会发布了却没验证能不能下载。

`scripts/install.sh` 是 `curl | bash` 用户实际执行的那份脚本，改完之后要跑一次 `release.sh` 才会同步到 S3——它不会单独发布，只跟着这个脚本一起上传。

上传的文件有四类，**顺序是有意的**：先是三个平台的二进制，然后是纯文本的 `version`（只有老 `install.sh` 在读它）和 `install.sh`，**最后才是 `manifest.json`**。`manifest.json` 带着版本号和每个目标的 sha256，是自动更新唯一的触发条件；先于二进制上去的话，已经在跑的 jms 会拿着新版本号去下还没替换完的旧文件，sha256 对不上，白失败一次。客户端拿不到 sha256 就**不更新**，不会降级成不校验——所以 `manifest.json` 缺失或格式坏了，表现是「没人收到更新」，不是「有人装了没校验的东西」。

`--build-only` 产出的 `dist/` 跟真上传的内容逐字节一致（包括 `version` 和 `manifest.json`），可以拿来核对再决定发不发。

需要本机配好 `aws` CLI 和有写权限的凭据；这一步会覆盖生产 CDN 上的公开分发内容，不接受 `-y` 时会先让你确认一遍目标列表。

## 构建

```bash
go mod download
go test ./...
go build -o jms ./cmd/jms
go build -o jmsd ./cmd/jmsd
```

这样构建出来的 `jms` 版本号是 `dev`，自动更新对它不生效——`dev` 解析不成 semver，客户端读不懂就不提示，正是从 clone 直接构建时想要的行为（本地代码不是「落后于发布版」，是跟发布版无关）。正式发布走 `scripts/release.sh <版本号>`，它会用 `-ldflags` 把真实版本号注进去。

`server` 和 `jmsd_addr` 没有内置默认值，构建完需要先写配置文件才能跑，见下面「客户端配置与日志」。

## JumpServer TUI

首次连接时，Koko 的 host key 会被自动记进 `~/.ssh/known_hosts`（和 `ssh -o StrictHostKeyChecking=accept-new` 一样），不需要提前手工 `ssh-keyscan`。

如果某天 jms 报 host key 不匹配，**不要删掉 `known_hosts` 里那一行重连**。这条只在「已经记过的 key 变了」时出现，正常原因只有 JumpServer 那边换过机器或重装过；先去跟运维确认，确认之后再删。

```bash
./jms

# 临时换一台服务器或一个 jmsd（都会盖掉配置文件里的值）：
./jms --server another-jumpserver.example.com
./jms --jmsd-addr https://jmsd.example.com
```

可选参数：

```text
--server <主机名>    # JumpServer 地址；不填走配置文件（必须有一个）
--jmsd-addr <URL>    # jmsd 地址；同上，正式部署要在配置文件里填
--identity <私钥路径>
--ssh-port 2222
--theme k9s          # 配色主题：k9s（默认）/ light / mono
--cache-ttl 12h      # 本地资产缓存有效期，0 关闭；按 r 总是强制刷新
--k9s-path <k9s 可执行文件路径>  # 不填就从 PATH 查找本机 k9s
--config <路径>      # 客户端配置文件，见下
--log-file <路径>    # 覆盖配置文件里的 log_file
-c "<类型> [名称]"   # 快捷进入，见下
--account <用户名>   # 配合 -c "<类型> <名称>" 指定用哪个账号
```

### 快捷进入（`-c`）

沿用 `mfmc` 的写法，整个参数是**一个带引号的值**：

```bash
./jms -c "ec2"                      # 进 TUI，已经筛到 ec2（等价于进去再敲 :ec2）
./jms -c "ec2 ng0"                  # 不进 TUI，直接连 ng0
./jms -c "ec2 ng0" --account root   # 直接连，并指定账号
./jms -c "k8s prod-cluster"         # 直接拉起本机 k9s
```

类型是 `all` / `ec2` / `rds` / `redis` / `k8s`（`kubernetes` 也认）。

**名称是精确匹配**，区分大小写，没有模糊和前缀匹配：没连上任何东西的时候没人在旁边看着，猜错了就是一张真实的、会进审计的连接票。匹配不到、或同名匹配到多个（JumpServer 允许重名），都会列出候选然后退出，不会替你挑。

**账号**：

- 只有一个可用账号 → 直接用
- 多个 → 列出来让你按数字选（读 `/dev/tty`，跟 stdin/stdout 有没有重定向无关）
- 多个 + 没有终端可问（cron、CI、systemd）→ 报错要求显式传 `--account`

`--account` 比对的是**用户名**（`root`、`ubuntu`），不是 JumpServer 里的账号别名。这里不设「默认第一个」：jmsd 返回的账号顺序不保证稳定，同一条命令今天用普通账号、明天用高权限账号，是审计里最不该出现的事。

需要 MFA 时会在终端上直接问验证码。**没有 `--totp` 参数**——验证码只有 30 秒有效期，写进命令行既进 shell history 又多半已经过期。

退出码（给脚本用）：

| 码 | 含义 |
|---|---|
| 0 | 正常结束 |
| 2 | 没出票，什么都没发生：参数错、名字没匹配上、账号不明确、本机没有 k9s、出票前取消了 MFA |
| 3 | 出过票，可能已经留下一条 Core 会话记录：出票失败、连 Koko 失败、会话本身出错 |

### 客户端配置与日志

配置文件默认在 `~/.config/jms/config.yaml`（macOS 是 `~/Library/Application Support/jms/config.yaml`）。整个文件可选，不存在就全默认，模板见仓库里的 `jms.example.yaml`：

```yaml
server: "jumpserver.example.com"     # 必填
# jmsd_addr: "https://jmsd.example.com"       # 正式部署必填，见下
log_level: "info"        # info（默认）| debug
# log_file: "/tmp/jms.log"
# disable_update_check: true                  # 关掉启动时的更新提示
# update_url: "https://.../jms"               # 换一个发布源，排练发布时用
```

**`server` 和 `jmsd_addr` 的优先级是「命令行 > 配置文件 > 编译期注入的默认值」。** 这两条写进配置文件就不用每次带参数了——判断命令行有没有传是看参数**有没有出现**，不是拿值跟默认值比，所以显式传一个刚好等于默认值的地址也能盖住配置文件。

**`server` 和 `jmsd_addr` 这两项必须有值**，源码里没有内置默认——任何一个写死的地址都只对一套部署成立，装完直接连上别人的机器不是能接受的默认行为。两项缺任一，`jms` 在 TUI 起来**之前**退出并告诉你改哪个文件（不是进了界面才报错，那时终端已经交出去了）。

要让本组织装完零配置可用，在构建时注入，不必改源码：

```bash
go build -ldflags "\
  -X github.com/leiyupeng/jumpserver-cli/internal/cli.defaultServer=jumpserver.example.com \
  -X github.com/leiyupeng/jumpserver-cli/internal/cli.defaultJmsdAddr=http://jmsd.example.com:8391" \
  -o jms ./cmd/jms
```

要连自己本机调试的 jmsd 就传 `--jmsd-addr http://localhost:8391`，或者在配置文件里写。前面挂了 TLS 就写 `https://`，jms 走标准 `net/http`，用系统信任库校验证书。

日志级别只走配置文件，没有对应的命令行参数，`disable_update_check` 和 `update_url` 也一样。

`disable_update_check: true` 之后启动时不会再有任何更新提示，也不会去连 CDN。`update_url` 换的是自动更新的发布源（默认就是上面 `install.sh` 那个 CDN 地址）——把某一台笔记本指到一个临时桶上，可以在不惊动其他人的前提下排练一次发布。**这两项写成「关掉」而不是「打开」是故意的**：没写这一项、或者干脆没有配置文件，检查都是开着的。

`info` 只记错误和关键事件——登录、会话开始/结束。`debug` 额外记每个 jmsd 请求和 connect 每一步的耗时，「卡在哪一步」用它。

TUI 占着终端没法往屏幕上打日志，所以写文件，默认 `~/Library/Caches/jms/jms.log`（Linux `~/.cache/jms/jms.log`），启动时会在 stderr 上告诉你路径。超过 4 MiB 截断重来，不轮转——它是排查当前这次的辅助，不是审计记录，审计看 Core 的 `terminal_session`。

K8s 资产始终启动用户电脑上的 K9s。代理地址和证书设置统一放在 `config-jmsd.yaml`；缺少代理配置或本机找不到 K9s 时直接报错，不会进入 Koko 远程 shell——**这两项都在出票之前检查**，不会白占一张连接票和一条 Core 会话记录。`--k9s-path` 指过去的文件**没有执行权限也算「找不到」**，同样在出票前就报错（从 release tarball 手动解出来的 k9s 常常缺这一位，以前这种情况能通过检查、到真要 exec 时才失败——那时票已经出了）。

Koko 那边的会话如果在 K9s 跑着的时候断了（Koko 重启、网络掉线、空闲超时），jms 会**先给 K9s 发 SIGTERM**、三秒后才强杀，让它有机会把终端交还回来、并把自己那份「上次看的是哪个视图、哪个 namespace」的状态写下去（这份状态是按 context 名分开存的，见下一段的 kubeconfig 改写）。

`insecure_skip_verify` 该不该开，先看清楚 `:5002` 上到底是什么证书：

```bash
echo | openssl s_client -connect <你的 proxy_url 主机>:5002 2>/dev/null \
  | openssl x509 -noout -subject -issuer -ext subjectAltName
```

- **`issuer` ≠ `subject`，`subjectAltName` 里只有域名** —— 证书本身没问题，是**用 IP 访问导致 SAN 不匹配**。把 `proxy_url` 从 IP 改成证书里那个域名，`insecure_skip_verify` 就可以关掉。这是最常见的情况
- **`issuer` == `subject`** —— 真自签名。要么在 LB 上换成正经证书，要么把签发 CA 分发到每台客户端机器的系统信任库
- **`issuer` 是你们内部 CA** —— 客户端机器必须先信任这个 CA，否则关掉 `insecure_skip_verify` 会直接连不上

注意 jms 生成临时 kubeconfig 时会**丢掉远端 kubeconfig 里的 `certificate-authority-data`**：那是 Koko 后端自己证书的设置，跟你 `proxy_url` 指向的地址（通常是 LB）的证书是两回事。所以这条链的校验只能靠系统信任库，上面第二、三种情况都得先把 CA 装上。

开着 `insecure_skip_verify: true` 意味着这条连接**加密但不验证对端身份**——中间人可以接。kubectl 流量里有 YAML 明文和 exec 终端流，跨公网时值得处理；两边都在同一内网时它是个不完美但可接受的折中。

配色主题：`k9s` 是默认，照着 K9s 自己的 stock skin 配的（dodgerblue 边框、orange 强调色、aqua 表头）；浅色终端用 `light`；`mono` 只用终端自身的颜色和粗体，适合自定义了配色方案或不支持真彩色的终端。三套主题的背景都不刷底色，沿用终端自己的背景。给一个不存在的名字会直接报错并列出可选值，不会静默回退。

启动后没有登录页，也没有任何输入框：`jms` 本地读一下私钥、算出这把公钥的 SHA256 指纹，就直接问 jmsd「这个指纹是谁」（`GET /v1/whoami`），拿到用户名后直接进工作台去问 jmsd 要资源列表——全程不需要用户输入任何东西。只有 jmsd 判断你超过 `mfa_interval_days` 没验证过，才会弹 MFA 验证码框（见下），首次 `/v1/whoami` 也会走这条路径。真正跟 Koko 打交道，只发生在你选中一个资产按 Enter、jmsd 出票之后那一次。Server 和 identity 是 CLI 参数。

指纹反查是怎么做到的：JumpServer 把每个用户的 SSH 公钥加密存在 `authentication_sshkey` 表里，且用的是随机 nonce 的 AES-GCM——相同明文每次加密出来的密文都不一样，所以没法建索引直接查，只能全表拉下来逐把解密、算指纹比对。听着重，其实很轻：一家公司的活跃公钥数通常是几十到几百量级，全部解密+比对在个位数毫秒。如果同一把公钥被注册到了两个不同用户名下（JumpServer 对这个字段没有唯一约束，理论上能发生），jmsd 会拒绝反查而不是随便认一个，报「this SSH key is registered to more than one JumpServer user」。

**这张表在 jmsd 内存里缓存 30 秒**，指纹也预先算好，所以上面那次扫描是每 30 秒一次、而不是每个请求一次。这不是为了省那几毫秒——**验签必须在反查之后**（不先知道是谁，就不知道该拿哪把公钥去验），所以在缓存之前，任何能连上端口的人只要发一个签名乱填的请求，就能让 jmsd 对 Core 的生产库跑一次全表查询加逐行解密：一个不要钱的请求换一次很贵的查询，打的还是 JumpServer 自己依赖的那个库。缓存之后伪造请求只值一次 map 查找，放大没了。

代价是 30 秒的滞后，两个方向都有：**刚在网页上加的公钥，最多 30 秒后才认；刚停用的公钥，最多 30 秒内还能用。** 后者是真正要在意的那个方向，所以窗口取得比攻击者本来就要在里面活动的时间戳/nonce 窗口更小。想立刻生效就重启 jmsd。缓存刷新会在 `log_level: debug` 下打一行，带公钥条数和耗时——「jmsd 到底看到我新加的 key 了没」只有这一行能回答。

签名协议也相应变了：以前每个请求把用户名和时间戳、随机数一起签名（`username|ts|nonce`），现在把用户名换成了公钥指纹（`fingerprint|ts|nonce`）——指纹不是秘密，它就是公钥的哈希，`ssh-keygen -lf` 谁都能算，跟明文用户名一样可以放心放在 URL 里传。

## jmsd（控制服务）

`jmsd` 是 `jms` 用来做 MFA 间隔控制和资产查询的控制面服务（完整设计见 `docs/PLAN.md`）。它持有一个专用的 JumpServer 账号凭据，普通用户全程看不到、也不需要自己创建任何凭据。

```bash
go build -o jmsd ./cmd/jmsd
go build -o jmsd-admin ./cmd/jmsd-admin
```

### 1. 建 jmsd 的账号

**不用 JumpServer 的 service account 概念**（`is_service_account` + `BOOTSTRAP_TOKEN` 那一套）——查过了，这条路全网没有公开文档，JumpServer 自己的 Koko、官方 sdk-go 都不用它，是个孤立、没人兜底的功能，说改就能改（详见 `docs/PLAN.md` §5.B、§11）。改用**普通账号 + 个人 Access Key**，代价是中间多一步没法脚本化的人工登录（JumpServer 没有任何接口能替别人生成 Access Key，只能自己给自己建）。

用 `jmsd-admin`（本仓库自带的一次性运维工具，只用来做这几步，不随 `jms`/`jmsd` 部署）：

```bash
export JMSADMIN_CORE_HOST="https://jumpserver.example.com"
export JMSADMIN_ACCESS_KEY_ID="<你自己的管理员 Access Key id>"
export JMSADMIN_ACCESS_KEY_SECRET="<你自己的管理员 Access Key secret>"

# 1. 建一个普通账号，带一次性临时密码
./jmsd-admin create-user --username jmsd --email jmsd@<your-domain>
```

这一步会打印临时密码和后续手动步骤的说明：

1. 用打印出来的临时密码登录一次 JumpServer 网页，去个人页面的 **API 密钥** 页面建一个，复制 `id`/`secret`——这两个就是下面 jmsd 配置文件里的 `access_key_id`/`access_key_secret`
2. 把临时密码作废，账号从此登不上网页，但已建的 Access Key 不受影响：

   ```bash
   ./jmsd-admin lock-account --username jmsd
   ```

3. 建（或复用）最小权限角色并绑定，替换掉默认的 `system_user`/`org_user`：

   ```bash
   ./jmsd-admin setup-role --username jmsd
   ```

   只给三条权限：`authentication.add_superconnectiontoken`（开票）、`perms.view_userassets`（查别人的权限树/资产）、`users.view_user`（按用户名查用户）。**不给** `authentication.view_superconnectiontokensecret`——那个权限能让调用方读到目标资产密码，jmsd 用不上。

**License 提醒**：普通账号不像 service account 那样被排除在用户统计之外，如果你们的 JumpServer 是按人头收费的版本，这个账号可能占一个 License 名额。

**轮转**：`create-user`（换个新名字）→ 人工建 key → `lock-account` → `setup-role` → 确认新账号能用后 `./jmsd-admin delete-user --username jmsd` 删掉旧的。普通账号支持标准 DELETE，不会有清不掉的孤儿账号。

### 2. 运行 jmsd

配置走一个 YAML 文件（默认 `/etc/jmsd/config.yaml`，可用 `--config` 或 `JMSD_CONFIG` 换路径）。除了上一步拿到的 Access Key，jmsd 还要**直连 JumpServer Core 的 PostgreSQL**（只读账号即可）+ Core 的 Django `SECRET_KEY`，用来验证用户的 SSH 公钥和 TOTP 验证码——原因和加密方案细节见 `docs/PLAN.md` §4/§5.C/§5.D/§7.2。这个文件本身含密钥明文，权限**必须** `chmod 600`，且不要提交进版本库：

```yaml
# /etc/jmsd/config.yaml
listen: ":8391"
core_host: "https://jumpserver.example.com"
access_key_id: "<access_key.id>"
access_key_secret: "<access_key.secret>"
core_db_dsn: "postgres://readonly_user:pass@core-db-host:5432/jumpserver?sslmode=disable"
inventory_db_dsn: "jmsd_readonly:pass@tcp(mysql-host:3306)/cloud_meta?parseTime=true&loc=UTC"  # 可选，见下方「云元数据（d 键）」
secret_key: "<JumpServer Core 自己 config.txt 里的 SECRET_KEY，原样抄>"
mfa_interval_days: 10   # 可选，默认 10
log_level: "info"       # 可选，info（默认）| debug；debug 会记每次库查询和 Core API 调用的耗时
k8s:                     # K8s 连接必填；K9s 始终运行在用户电脑
  proxy_url: "https://jumpserver.example.com:5002"  # 办公网能访问到的 Koko :5002 地址
  insecure_skip_verify: false                 # 见下：先确认证书情况再决定要不要开
redis:                   # 可选，见下方「MFA 状态存哪」
  addr: "127.0.0.1:6379"
  db: 0
```

```bash
./jmsd --config /etc/jmsd/config.yaml
```

#### MFA 状态存哪

jmsd 记着每个人上次通过 MFA 的时间，用来判 `mfa_interval_days`。**不配 `redis:` 就存在进程内存里，jmsd 一重启记录清零，所有人都要重验一次。** 配上 `redis:` 之后这条记录（以及 30 秒的防重放 nonce）落到 Redis，重启不再影响：

```
127.0.0.1:6379> KEYS jmsd:*
1) "jmsd:mfa:zhangsan"        # 值是 unix 时间戳，TTL = mfa_interval_days
2) "jmsd:nonce:k3Jd8Fq2..."   # 防重放，30 秒后自动消失
```

**Redis 必须 `bind 127.0.0.1`。** 这里不支持密码和 TLS，假定的就是一个本地无认证 Redis。存进去的都不是秘密——用户名、时间戳、随机数，TOTP 密钥和连接票一概不碰——但反过来是真风险：**能连上这个 Redis 的进程可以自己写一个 `jmsd:mfa:<用户名>`，等于替那个人跳过 MFA**。

配了 `redis:` 但启动时连不上，jmsd **直接启动失败**，不会静默退回内存——否则「怎么又要我验 MFA」就成了一个没人查得出来的谜。运行中 Redis 挂掉，请求返回 503，而不是伪装成认证失败。

**这个数据库账号除了 `SELECT`，只需要在 `assets_favoriteasset` 一张表上额外给 `INSERT`/`DELETE`**——那是「收藏」唯一要写的地方，jmsd 不写库里任何其他内容：

```sql
GRANT SELECT ON ALL TABLES IN SCHEMA public TO jmsd_readonly;
GRANT INSERT, DELETE ON assets_favoriteasset TO jmsd_readonly;
```

收藏为什么走数据库而不是 REST API：`/api/v1/assets/favorite-assets/` 是按**调用方**身份返回的，也就是 jmsd 自己那个服务账号的收藏，加 `user_id`/`user` 过滤参数无效（v4.10.16 实测），拿不到「某个用户收藏了什么」。`assets_favoriteasset` 表可以，而且读的就是网页控制台那份数据，两边天然同步。`◷ Recent` 同理读 `terminal_session`，顺带免掉给 jmsd 的角色加 `terminal.view_session` 权限。

`secret_key` 是 JumpServer 全库加密字段的主密钥，不止 jmsd 读的这两个字段；把它和只读账号搭配、文件权限锁死，是本方案能收窄这个风险的全部手段。

#### 云元数据（`d` 键）

`inventory_db_dsn` 不填就是不开这个功能，其余一切照旧，按 `d` 会明说「jmsd 没配 inventory 数据库」。填了但连不上，jmsd **启动失败**——既然配了就是要用，静默把功能吞掉比起不来更难查。

表是姊妹项目 cloud-sync（jumpserver-tool）建的 `cloud_instance_info`，它写、jmsd 只读：

```sql
GRANT SELECT ON cloud_meta.cloud_instance_info TO 'jmsd_readonly'@'%';
```

**DSN 必须带 `parseTime=true&loc=UTC`。** 那几个时间列是 `DATETIME`、本身不带时区，存的是 UTC；不写 `loc` 的话驱动会当成本地时间读，面板上每个时间都会静默差一个时区。

**jmsd 启动时把整张表读进内存**，之后所有请求都从内存里拿，一次都不会为这张表等：快照过期（5 分钟）时先把手上这份返回，刷新在后台另起一个 goroutine 做。表在另一个 region，一次加载一秒多——放在守护进程启动时没人在看，放在请求路径上就是每个开 jms 的人在等（而且没人会 5 分钟内开两次，等于每次都撞上）。

资产 comment 里的 `[cloud-sync:...]` 是资产和元数据之间唯一的连接。`/v1/assets` 解出它、在快照里确认有行，然后只把 key 发下去；`/v1/assets/describe` 拿 key 做一次 map 查找。`comment` 字段是白拿的——它本来就在 `AssetPermedSerializer.only_fields` 里（v4.10.16 `user_permission.py:35`），之前只是被丢掉了。

**describe 不再按调用者权限校验那个 key**，这是有意的取舍：校验就意味着每按一次 `d` 问一次 Core，正好是这个接口存在的意义所在。还成立的部分：签名保证调用者是个已知的 JumpServer 用户，而 key 只随着该用户有权限的资产发下去，所以拿到别的机器的 key 得靠别的途径。里面装的是规格、标签和网络 id，不是任何凭据。

数据库挂掉时**继续用内存里那份**（和 SSH 指纹索引那份缓存的规矩相反：那份是拿来鉴权的，宁可失败也不能用旧的；这份只是给人看的，为一次抖动把面板弄没更亏）。启动时就连不上也只是元数据为空，`/v1/assets` 照常返回——资产列表是整个工作区，不能被一个只用来显示的库拖下水。

### 已实现的接口

```text
GET  /v1/whoami        ?fp=&ts=&nonce=&sig=                            -> {username, server_version, jumpserver_baseline}
GET  /v1/k8s-config    ?fp=&ts=&nonce=&sig=                            -> {proxy_url, insecure_skip_verify}（来自 config-jmsd.yaml 的 k8s 段，全员共用同一份）
GET  /v1/nodes         ?fp=&ts=&nonce=&sig=                            -> [{id, name, children}]，权限树，嵌套 JSON
GET  /v1/assets        ?fp=&ts=&nonce=&sig=                            -> [{id, name, address, category, type, node_ids, cloud_key}]
                       （一次返回全部有权限的资产，没有 node 过滤参数：scope/分类/搜索都在客户端对这一份做，
                         左树的每节点计数也是从它算出来的，按节点取子集反而算不出别的节点的数。
                         `cloud_key` 指向该机器的云元数据，配了 inventory_db_dsn 才有；空表示没有，客户端据此不再发 describe）
GET  /v1/assets/detail ?fp=&ts=&nonce=&sig=&asset=<id>                 -> {accounts, protocols}
GET  /v1/assets/describe ?fp=&ts=&nonce=&sig=&key=<cloud_key>          -> 一台机器的云元数据，纯内存 map 查找
GET  /v1/marks         ?fp=&ts=&nonce=&sig=                            -> {favorites: [id], recent: [id]}
POST /v1/favorites     {fp, ts, nonce, sig, asset, starred}            -> {"status":"ok"}
POST /v1/auth/mfa      {fp, ts, nonce, sig, code}                      -> {"status":"ok"}
POST /v1/connections   {fp, ts, nonce, sig, asset, account, protocol} -> 连接票据
```

`fp` 是调用方 SSH 公钥的 SHA256 指纹（`ssh.FingerprintSHA256`），不是用户名——jmsd 先按指纹反查出这是哪个用户（`userDirectory.Lookup`，查的是那份 30 秒缓存的指纹索引，见上一节），再验签。`sig` 是用户本地 SSH 私钥签名后 base64 的结果——`/v1/auth/mfa` 把 `code`、`/v1/connections` 把 `asset|account|protocol` 一并绑进签名内容，防止拿别的请求的签名跨接口重放。`jms` 内部的 `internal/jmsdclient` 自动处理签名。反查出用户后，jmsd 再拿这个用户在 JumpServer 里登记的**所有生效 SSH 公钥**逐把验签，只要有一把过就放行。所有接口（`/v1/auth/mfa` 自己除外——那正是用来重新验的）都会在 MFA 超过 `mfa_interval_days` 时返回 401 `{"mfa_required":true}`，`jms` 会自动弹验证码输入框——包括 `/v1/whoami` 本身，首次调用就可能要求先验一次 MFA。

选中资产按 Enter：`jms` 先调 `/v1/assets/detail` 拿该资产的可用账号/协议，账号只有一个就直接用、多个弹出选择框；协议里只要有 `ssh` 就默认用 `ssh`（不弹框，即使同时还挂着 `sftp` 等其他协议），没有 `ssh` 时才在多个协议之间弹框选择；再调 `/v1/connections` 拿一张连接票据，用 `JMS-<票号>`/`<票值>` 直接连 Koko 进入会话——跟阶段 0 手工验过的 `JMS-<票号>` 那条路完全一样，只是自动化了。

`jms` 通过配置文件的 `jmsd_addr`（或 `--jmsd-addr`）找到 jmsd，没有内置默认值。本机调试指到自己的 jmsd：`--jmsd-addr http://localhost:8391`。

**jmsd 自己不做 TLS**（用的是 `ListenAndServe`，没有证书配置项）。要加密就在前面挂 nginx/caddy 之类的反向代理，同时把 `listen` 从 `:8391` 改成 `127.0.0.1:8391`，这样明文端口不对外暴露，客户端那边写 `jmsd_addr: "https://..."`。**票据是在这条连接上明文返回的**，所以跨公网时这一步是必要的；jms 拿票之后连 Koko 的那条本来就是 SSH，已经加密并且会校验 `known_hosts`，不受影响。

jmsd 每次拒绝请求都会在自己的 stderr 打一行 `jmsd: <method> <path> user=<用户名> -> <状态码> <原因>`，客户端看到什么错，服务端就有对应的一行（签名本身不进日志）。

### 版本不一致会提示更新

`GET /v1/whoami` 顺带把 jmsd 那边的两个版本号报回来，`jms` 跟自己编进去的一比，**不一样就在顶部状态行末尾用黄字提示**，同时写进 `jms` 的日志。查两样：

| 查的是 | 不一致时显示 | 说明 |
|---|---|---|
| 构建版本 | `jms 需更新:本地 X / jmsd Y` | `jms`/`jmsd` 从同一个仓库一起发布、共用 `internal/version.Version`，所以不一样 = 两个二进制来自不同构建，通常就是笔记本上的 `jms` 一直没重新拷 |
| JumpServer 基线 | `JumpServer 基线不一致:jms X / jmsd Y` | 两边**各自**声明自己是按哪个 JumpServer 版本验的：`jms` 用 `internal/version.JumpServerBaseline`，`jmsd` 用 `internal/jmsd.JumpServerBaseline` |

两个都不一致就都显示，用 `·` 隔开，构建版本排前面（那个更可操作）。

JumpServer 基线故意是**两个独立常量而不是一个共享的**——共享常量不可能跟自己不一致，也就永远报不出东西来。改 JumpServer 版本时两处一起改（有个测试盯着它们别漂移）。

用 `/v1/whoami` 而不是单开一个接口，是因为它本来就在启动时调恰好一次、本来就永不走缓存——**零额外请求、零额外延迟**。

提示放在顶部那一行而不是状态栏，是因为状态栏的内容会被下一次状态更新冲掉（加载完成、切 scope 都会），几秒钟后就没了；顶部这行每次重绘都重新拼，提示能一直留着。

**旧版 jmsd 不带这两个字段，收到空值就什么都不提示**——空值意味着"对面答不上来"，不等于"版本不一致"，拿它报警会导致每次连老 jmsd 都弹一次假警报。两个字段各自独立判断：只报了构建版本、没报基线的 jmsd，构建版本那条照查。

版本号是手改的常量，不从 git 推导：`internal/version/version.go`（构建版本 + jms 侧基线）和 `internal/jmsd/config.go`（jmsd 侧基线）。

### MFA 验证码有次数限制

**同一个用户连续输错 5 次验证码，`/v1/auth/mfa` 会在之后 15 分钟内一律返回 429**，错误文案带剩余时间（`too many incorrect MFA codes; try again in 14m52s`），`jms` 会把它显示在验证码输入框下面。输对一次就清零——数的是**连续**失败，偶尔手滑不会跨天累积。锁定生效的那一刻会在 stderr 打一行 `jmsd: mfa lockout: <用户名> reached 5 consecutive incorrect codes`。

为什么必须有：验证码 6 位、且校验接受 3 个时间窗，单次猜中概率约 3/10⁶。不限次数的话这不叫防护——拿到某人 SSH 私钥的人可以无限次发合法签名的请求，一秒 100 次的话期望一个多小时就猜进去了。而 MFA 存在的意义恰恰就是「私钥已经泄露」这个场景。限成 5 次/15 分钟之后同样的穷举要几百年。

按**用户**限，不按来源 IP：能走到这一步的人已经持有能签成这个用户的私钥，账号是唯一他改不掉的标识，而 IP 在反向代理后面会退化成所有人共用一个。计数存在 jmsd 进程内存里，重启清零——这一条不像 MFA 时间戳那样需要持久化，因为忘掉锁定只对「能重启 jmsd 的人」有利，而那种人早就有别的路子了。

「这个用户没登记 MFA 设备」不计入失败次数：那不是在猜，锁了只会盖住真正的原因。

## 当前限制

- **只支持 macOS / Linux，Windows 编译不过**：`internal/sshclient/gateway.go` 用 `SIGWINCH` 同步终端窗口大小，这个信号 Windows 没有。Windows 用户请用 WSL2（原生 Linux 二进制，零移植风险），或者 SSH 到一台跑着 `jms` 的 Linux 机器上用——后者要注意每人必须是独立的系统账号，否则 `LoadIdentity` 会把 `~/.ssh` 里第一个私钥当成所有人的身份
- 还不支持加密的私钥；请提供一个权限设置合理的未加密私钥。不过带口令的 key **不会再挡住后面的候选**：`~/.ssh/id_rsa` 带口令、`~/.ssh/id_ed25519` 没带的话，直接用后者（之前会在第一把上硬失败、报「encrypted private keys are not supported」，压根不去试第二把）。两把都用不了时，报的是那把存在但用不了的 key 的错，而不是笼统的「no private key found」——要改的就是那个文件
- jmsd 不配 `redis:` 时，MFA 状态存在进程内存里，重启后所有用户都要重新验一次；配上就不会了，见上面「MFA 状态存哪」
- 不支持"连接时手动输入账号密码"（`@INPUT` 别名的账号）——`jmsd` 直接把这类账号从选项里过滤掉，见 `docs/PLAN.md` §5.F.30
- 不支持 Chen（Web GUI 资产）的数据库查询执行
- 不提供 SOCKS5。旧的 `jms socks5` 子命令已移除

## 安全

`jmsd` 持有 JumpServer Core 的 Access Key、只读库账号和 Core 的 `SECRET_KEY`，部署时请确认：

- `jmsd` 的配置文件 `chmod 600`，且不在版本库里（本仓库的 `.gitignore` 已覆盖 `config-jmsd.yaml` / `jmsd.yaml`）
- 数据库账号只给 `SELECT`，外加 `assets_favoriteasset` 一张表的 `INSERT`/`DELETE`
- Redis（如启用）必须 `bind 127.0.0.1`——能写它的进程可以替任何人跳过 MFA
- `jmsd` 只讲明文 HTTP，出内网前必须挂 TLS 代理

发现漏洞请私下报告给仓库 owner，不要开公开 issue。

## License

[MIT](LICENSE)
