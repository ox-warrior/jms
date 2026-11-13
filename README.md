# jumpserver-cli

JumpServer 的命令行客户端：一个 K9s 风格的终端界面，外加给脚本和 AI 用的非交互模式（远程执行命令、只读查数据库）。已在 JumpServer / Koko `v4.10.16` 上验证。

仓库产出两个程序：

| 程序 | 跑在哪 | 做什么 |
|---|---|---|
| `jms` | 用户的电脑（macOS / Linux） | 终端界面、`-c` 快捷连接、远程执行、查库、隧道 |
| `jmsd` | 内网一台机器 | 持有 JumpServer 的高权限凭据，负责认人、查权限、出连接票、执行只读查询、写审计 |

```mermaid
flowchart LR
    jms["jms<br/>(用户电脑)"] -- "本机钥匙签名的 HTTP" --> jmsd["jmsd<br/>(内网)"]
    jmsd -- "Access Key：查权限、出票" --> core["JumpServer Core"]
    jmsd -. "只读：启用用户、收藏、最近" .-> pg[("Core PostgreSQL")]
    jmsd -. "钥匙绑定" .-> redis[("Redis")]
    jms -- "SSH：JMS-票号 / 票值" --> koko["Koko"]
    koko --> target["目标资产"]
    jmsd -- "只读查询（经网域网关）" --> db[("数据库")]
```

- **不输密码**：`jms` 第一次运行时在本机生成一把钥匙，浏览器登录一次 Google，`jmsd` 按邮箱把钥匙绑到同名的 JumpServer 用户，10 天有效。
- **密码不出 Core**：连服务器用的是 `jmsd` 出的一次性票据，目标机密码只在 Core 和 Koko 之间流转。查数据库时由 `jmsd` 取密码并在自己进程里执行，`jms` 始终碰不到。
- **权限沿用 JumpServer**：能看到哪些资产、用哪个账号，全按你在 JumpServer 里的授权；在 JumpServer 里禁用用户，30 秒内失效。

## 安装

```bash
curl -fsSL "$JMS_CDN_BASE/install.sh" | bash
```

`$JMS_CDN_BASE` 是你们自己的发布地址（见下方「构建与发布」）。支持 macOS（arm64 / x86_64）和 Linux（x86_64 / arm64），自动识别。

- **不要加 `sudo`**。jms 装在 `~/.jms/bin/jms`，`/usr/local/bin/jms` 是指向它的软链接，只有建链接时脚本会自己要一次密码。这样二进制和配置都归你自己，以后自动更新不需要 sudo。（加了也能装对，脚本会把文件交还给你。）
- 安装会写好配置文件里的 `server` / `jmsd_addr` / `update_url` 三项，装完直接 `jms` 就能用。重装会把这三项覆盖成最新地址，你自己加的其他配置项保留。

### 自动更新（强制）

`jms` 每次启动（界面、`-c`、`-w` 都算）先查一下 CDN 有没有新版，有就**直接升级，不询问**：下载、校验 sha256、替换，然后带着原来的参数重新启动。

- 升级失败就不启动，打印原因并提示联系 leiyupeng。最常见的原因是早期用 sudo 装的 jms 属于 root，按提示重装一次即可。
- 连不上 CDN（不在公司网络、CDN 故障）照常启动，最多多等 2 秒。
- 本地版本不低于 CDN 时不动；源码直接编译的 `dev` 版本永远不升级。

## 使用

### 终端界面

```bash
jms
```

第一次运行会打开浏览器登录 Google（终端里也会打印链接），之后 10 天内不用再登录。

| 按键 | 作用 |
|---|---|
| `:ec2` `:rds` `:redis` `:k8s` `:all` | 按资源类型切换（默认 `:ec2`；`:host` 等同 `:ec2`） |
| `/` | 搜索，覆盖当前视图显示的所有列；搜索期间跨全部有权限的资产 |
| `Esc` | 清掉分类和搜索，回到 `:all` |
| `Enter` | 连接选中的资产（账号/协议多于一个时弹框选择，有 `ssh` 时默认 `ssh`） |
| `y` | 收藏 / 取消收藏（和网页控制台同步） |
| `d` | 查看这台云主机的详情：机型、CPU/内存、磁盘、安全组、VPC、计费、标签 |
| `r` | 强制刷新资产列表 |
| 双击 | 复制：NAME 列复制名字，ADDRESS 列复制地址（仅 macOS） |
| `Tab` | 在左侧树和右侧表格之间切换 |
| `:q` / `Ctrl+C` | 退出 |

- 左侧是 JumpServer 的节点树，外加 `★ Favorites`、`◷ Recent`、`● All permitted` 三个入口。节点后面的数字是当前分类和搜索条件下的匹配数，没有匹配的节点会隐藏。
- `◷ Recent` 来自 Core 的会话记录，网页端和直连 Koko 的连接也算在内。
- K8s 资产会在本机拉起 K9s，经 Koko 的 `:5002` 反向代理连接，需要本机装有 `k9s`。
- 资产列表在本地缓存 12 小时（`--cache-ttl` 调整，`0` 关闭），`r` 随时强制刷新。登录状态和出票每次都实时检查。
- 配色主题：`--theme k9s`（默认）/ `light` / `mono`。
- 首次连接时，Koko 的 host key 会自动记进 `~/.ssh/known_hosts`。如果之后报 host key 不匹配，**不要直接删掉那一行**，先找运维确认 JumpServer 是否换过机器。

### 快捷连接（`-c`）

```bash
jms -c "ec2"                       # 打开界面并筛到 ec2
jms -c "ec2 web-01"                # 不进界面，直接连 web-01
jms -c "ec2 10.0.0.13"             # 用 IP 也行
jms -c "ec2 web-01" --account root # 指定账号
jms -c "k8s prod-cluster"          # 直接拉起本机 k9s
```

- 类型是 `all` / `ec2` / `rds` / `redis` / `k8s`。
- **名字或 IP 必须完整写对**，区分大小写，没有模糊匹配。匹配不到或匹配到多个（JumpServer 允许重名）会列出候选然后退出。
- 只有一个账号时直接用；多个时让你选；没有终端可问时（cron、CI）要求传 `--account`。`--account` 填的是用户名（`root`、`ubuntu`），不是账号别名。

退出码：`0` 正常；`2` 没出票（参数错、名字没匹配、账号不明确、取消登录等）；`3` 出过票但连接或会话失败。

### 远程执行命令（`-c ... -- 命令`）

给脚本和 AI 用，`--` 后面的内容在服务器上执行，和 `ssh host 命令` 一样：

```bash
jms -c "ec2 web-01" -- uptime
jms -c "ec2 web-01" --account deploy -- 'tail -n 100 /var/log/app.log | grep ERROR'
jms -c "ec2 web-01" -- 'cat > /tmp/x.conf' < x.conf     # stdin 会转发过去
```

- **整条命令用一对引号括起来**：`--` 后面的多个参数会用空格拼成一条交给远端 shell。
- 不分配终端，stdout 和 stderr 分开返回，可以直接接管道。
- **默认 60 秒超时、输出最多 1MiB**，超过就断开，退出码 255。`--timeout 5m` 放宽，`--timeout 0` 不限时；输出上限不可调。`tail -f`、大文件请在命令里自己带 `timeout`、`head`、`tail -n`。
- 只支持 SSH 资产。
- **这个模式从不提问**：没登录或登录过期时直接失败并提示让人去跑 `jms login`；有多个账号时要求传 `--account`。
- 每条命令出一张票，Core 里留一条会话记录（命令原文和前 1KB 输出）；命令原文同时记进 jmsd 的审计。
- 被 JumpServer 命令过滤规则拒绝时 Koko 不输出任何内容、退出码为 0。命令没有任何输出时 jms 会在 stderr 补一句提示。

退出码：远端命令的退出码原样透传；`255` 表示 jms 自己失败（参数错、没登录、名字没匹配、出票或连接失败、超时、输出超限），原因看 stderr。

### 查询数据库（`-c "rds 库名" -- 查询`）

由 jmsd 执行，**只读**。资产在 JumpServer 网域里时，jmsd 和 Koko 一样经网域网关转发。

**MySQL / PostgreSQL**：一条 SQL，输出格式同 `mysql -B`（首行列名、制表符分隔、`NULL` 原样、单元格里的制表符和换行转义成 `\t` `\n`）。

```bash
jms -c "rds staging-db" -- 'select id, status from orders.orders where id = 123'
jms -c "rds staging-db" -- 'show tables from orders'
```

- 只允许 `SELECT` `SHOW` `DESCRIBE` `EXPLAIN` `WITH`，在只读事务里执行，结束后回滚。
- 没有 `use`：MySQL 表名写成 `库.表`；PostgreSQL 连的是资产配置的库（没配是 `postgres`）。psql 的 `\d` 这类命令不支持，请查 `information_schema`。
- SQL 报错时 stderr 打印 `ERROR: ...`，退出码 1。

**Redis**：redis-cli 的写法，一行一个值（同 `redis-cli --raw`），嵌套列表（如 `SCAN` 的结果）以 JSON 写在一行。

```bash
jms -c "redis cache-01" -- 'HGETALL user:123'
jms -c "redis cache-01" -- 'SCAN 0 MATCH user:* COUNT 100'
```

- 只允许读命令（`GET` `HGETALL` `LRANGE` `SMEMBERS` `ZRANGE` `SCAN` 系列 `TTL` `TYPE` `INFO` `DBSIZE` `MEMORY USAGE` `SLOWLOG GET` 等）。**不允许 `KEYS`**（会卡住整个实例，用 `SCAN`），不允许 `CONFIG GET`（会返回密码）。
- 只连单个节点：集群模式下访问其他分片的 key 会返回 `MOVED`。

**MongoDB**（归在 `rds` 类型下）：写成 MongoDB 的**命令文档（JSON）**，不是 mongosh 的 `db.x.find()`。`"$db"` 指定库，不写用资产配置的库。输出一行一个文档，可以接 `jq`。

```bash
jms -c "rds mongo-01" -- '{"listDatabases": 1, "nameOnly": true, "authorizedDatabases": true}'
jms -c "rds mongo-01" -- '{"find": "events", "filter": {"uid": 123}, "limit": 10, "$db": "app"}'
jms -c "rds mongo-01" -- '{"aggregate": "events", "pipeline": [{"$group": {"_id": "$type", "n": {"$sum": 1}}}], "cursor": {}, "$db": "app"}'
```

- 只允许 `find` `aggregate` `count` `distinct` `listCollections` `listIndexes` `listDatabases` `dbStats` `collStats` `serverStatus` `ping` `buildInfo` `hello` `replSetGetStatus`；带 `$out` / `$merge` 的 `aggregate` 拒绝。

**所有数据库**：最多 1000 行（文档）、1MiB、30 秒，超了截断并在 stderr 提示；被拒绝或 jms 自己失败时退出码 255；每条查询记进 jmsd 审计（SQL 原文、行数、耗时）。

### 隧道（`-w`）

```bash
jms -w 9090                                        # 本机起一个 SOCKS5 代理
curl -x socks5h://127.0.0.1:9090 http://10.0.0.13:8080
```

流量从 jmsd 那台机器发出去，用来访问内网里没登记成资产的服务。需要 jmsd 开启隧道（见下方 jmsd 配置）。

- 只支持 SOCKS5 的 CONNECT（TCP），不支持 UDP。
- **Core 里没有任何记录**，只有 jmsd 的日志和审计记录了谁连了哪里、传了多少字节。

### 登录与给机器用的钥匙

```bash
jms login                  # 现在就登录一次 Google
```

- 人的身份是本机钥匙（macOS `~/Library/Application Support/jms/device_key`，Linux `~/.config/jms/device_key`），由 Google 登录绑定，**每台电脑各自登录**。钥匙删了重新登录一次就行。
- 非交互模式（`-c ... -- 命令`、查库）遇到登录过期不会弹浏览器，请人跑一次 `jms login`。
- **服务器上的程序（比如 AI）开不了浏览器**：给它生成一把钥匙，把公钥登记到 jmsd 的 `service_keys`，然后用 `-i` 指定私钥：

  ```bash
  ssh-keygen -t ed25519 -N '' -f ~/.jms/agent_key
  jms -i ~/.jms/agent_key -c "ec2 web-01" -- uptime
  ```

  这把钥匙永久有效、不走 Google 登录；未登记时直接报错，不会被绑到某个人身上。私钥不能设密码。

### 参数一览

```text
-c "<类型> [名字|IP]"     快捷进入 / 直接连接
-- <命令>                 配合 -c，在资产上执行命令或查询
--account <用户名>        配合 -c 指定账号
--timeout 60s             远程执行的超时，0 不限
-w <端口>                 起 SOCKS5 隧道
-i, --identity <私钥>     用登记在 service_keys 里的钥匙（给程序用）
--server <主机名>         JumpServer 地址，覆盖配置文件
--jmsd-addr <URL>         jmsd 地址，覆盖配置文件
--ssh-port 2222           Koko 的 SSH 端口
--theme k9s|light|mono    配色
--cache-ttl 12h           资产缓存有效期，0 关闭
--k9s-path <路径>         k9s 可执行文件，不填从 PATH 找
--config <路径>           配置文件
--log-file <路径>         日志文件

jms login                 登录 Google
jms version               版本号
```

### 配置与日志

配置文件：macOS `~/Library/Application Support/jms/config.yaml`，Linux `~/.config/jms/config.yaml`。安装脚本会写好，模板见 `jms.example.yaml`：

```yaml
server: "jumpserver.example.com"             # 必填
jmsd_addr: "http://jmsd.example.com:8391"    # 必填
update_url: "https://cdn.example.com/jms"    # 自动更新的发布源；排练发版时可指向临时桶
log_level: "info"                            # info | debug（debug 记每个请求的耗时）
# log_file: "/tmp/jms.log"
```

命令行参数优先于配置文件。日志默认在 macOS `~/Library/Caches/jms/jms.log`、Linux `~/.cache/jms/jms.log`，超过 4MiB 截断重写。

## 部署 jmsd

### 1. 建 jmsd 用的 JumpServer 账号

jmsd 用一个普通 JumpServer 账号的 Access Key 调 Core API。用仓库自带的一次性工具 `jmsd-admin` 建（需要一个管理员的 Access Key）：

```bash
export JMSADMIN_CORE_HOST="https://jumpserver.example.com"
export JMSADMIN_ACCESS_KEY_ID="<管理员 Access Key id>"
export JMSADMIN_ACCESS_KEY_SECRET="<管理员 Access Key secret>"

./jmsd-admin create-user --username jmsd --email jmsd@example.com   # 打印一次性临时密码
```

1. 用临时密码登录一次 JumpServer 网页，在个人页面的 **API 密钥** 里建一个 Access Key，记下 `id` / `secret`。
2. 锁掉网页登录（已建的 Access Key 不受影响）：`./jmsd-admin lock-account --username jmsd`
3. 绑定最小权限角色：`./jmsd-admin setup-role --username jmsd`

   角色包含：`perms.view_userassets`（查用户的权限和资产）、`authentication.add_superconnectiontoken`（出票）、`authentication.view_superconnectiontokensecret`（查库时取账号密码；不给的话 SSH 照常可用，只有查库失败）、`users.view_user`。

这个账号可能占用一个 JumpServer License 名额。轮换时建一个新名字的账号走一遍上面的步骤，确认能用后 `./jmsd-admin delete-user --username jmsd` 删掉旧的。

### 2. 数据库账号

jmsd 直连 Core 的 PostgreSQL，读启用用户、收藏和最近连接，只需要：

```sql
GRANT SELECT ON ALL TABLES IN SCHEMA public TO jmsd_readonly;
GRANT INSERT, DELETE ON assets_favoriteasset TO jmsd_readonly;   -- 收藏
```

### 3. 配置

默认读 `/etc/jmsd/config.yaml`（`--config` 或 `JMSD_CONFIG` 换路径）。文件里有密钥明文，**`chmod 600`，不要进版本库**。完整模板见 `jmsd.example.yaml`：

```yaml
listen: ":8391"
core_host: "https://jumpserver.example.com"
access_key_id: "<access key id>"
access_key_secret: "<access key secret>"
core_db_dsn: "postgres://jmsd_readonly:pass@core-db:5432/jumpserver?sslmode=disable"

google:                                  # 必填
  client_id: "xxxx.apps.googleusercontent.com"
  hosted_domain: "example.com"
mfa_interval_days: 10                    # Google 登录一次管几天，默认 10

redis:                                   # 强烈建议配，否则 jmsd 重启后所有人要重新登录
  addr: "127.0.0.1:6379"
  db: 0

service_keys:                            # 可选，给程序用的钥匙
  - user: "ai-agent"                     # 它代表的 JumpServer 用户名
    public_key: "ssh-ed25519 AAAA... ai-agent"
  - public_key: "ssh-ed25519 AAAA... gateway"   # 不写 user：只能替别人操作（见下）

audit_es: "http://es.example.com:9200"   # 可选，审计写到 Elasticsearch

k8s:                                     # 连 K8s 资产时必填
  proxy_url: "https://jumpserver.example.com:5002"
  insecure_skip_verify: false

inventory_db_dsn: "user:pass@tcp(mysql:3306)/cloud_meta?parseTime=true&loc=UTC"  # 可选，d 键的云元数据

tunnel:                                  # 可选，jms -w 用
  enabled: true
  listen: ":8392"
  host_key: "/etc/jmsd/tunnel_host_key"  # 不存在会自动生成
  # allow:                               # 不写 = 任意目标；写了只放行列出的
  #   - {cidr: "10.0.0.0/16", ports: [443, 3306]}
  require_permitted_asset: false         # true = 只能连自己有权限的资产地址

log_level: "info"                        # info | debug
```

各项说明：

- **`google`**：`client_id` 是 GCP 里「桌面应用」类型的 OAuth 客户端，必须和编进 jms 的是同一个。只接受 `hosted_domain` 这个 Workspace 下已验证的邮箱，且 JumpServer 里恰好有一个启用的用户用这个邮箱。jmsd 要能访问 `https://www.googleapis.com`（拉 Google 的签名公钥）。jmsd 不需要 client secret。
- **`redis`**：存钥匙绑定和防重放的随机数。**必须 `bind 127.0.0.1`**，不支持密码：能写这个 Redis 的进程可以把任意钥匙绑给任意人。配了但启动时连不上，jmsd 直接启动失败。要踢掉某台电脑，`DEL jmsd:key:<指纹>`。
- **`service_keys`**：公钥解析失败时 jmsd 启动失败。**不写 `user` 的钥匙自己没有任何权限**，每个请求都必须带 `--on-behalf-of <邮箱>`，jmsd 按这个人的 JumpServer 授权办事（这个人能连什么它就能连什么，Core 里的会话也记在这个人名下），不带就拒绝；这种钥匙能代表任何人，私钥要按这个分量保管。写了 `user` 的钥匙照旧：这把钥匙能连什么，完全由那个 JumpServer 用户的授权决定，建议单独建用户、只授权只读账号，并配上 JumpServer 的命令过滤。
- **`audit_es`**：每个动作写一条文档，每天一个索引 `jmsd-audit-YYYY-MM-DD`（北京时间），保留 30 天；索引模板和 ILM 策略由 jmsd 启动时自动创建。后台每 5 秒批量写一次，ES 故障不影响使用，写失败的事件会打到 jmsd 日志里。

  | `action` | 时机 | 主要字段 |
  |---|---|---|
  | `ticket` | 出票（SSH、k8s 等） | `asset` `account` `protocol` `ticket`，远程执行时带 `command` |
  | `query` | 执行一条查询 | `asset` `asset_name` `account` `gateway` `sql` `rows` `ms` |
  | `signin` | Google 登录 | `email` `fingerprint` |
  | `tunnel.open` / `tunnel.close` / `tunnel.stream` | 隧道连接、断开、每条流 | `requested` `target` `bytes_up` `bytes_down` `streams` |

  每条都有 `@timestamp` `action` `status`（`ok` / `denied` / `error`）`user` `remote` `host`，失败时带 `reason`。不记查询结果和密码。

- **`k8s`**：`proxy_url` 是办公网能访问到的 Koko `:5002` 地址。要不要开 `insecure_skip_verify`，先看证书：`echo | openssl s_client -connect <主机>:5002 2>/dev/null | openssl x509 -noout -subject -issuer -ext subjectAltName`。常见情况是用 IP 访问导致证书域名不匹配，把 `proxy_url` 改成证书里的域名即可关掉。
- **`inventory_db_dsn`**：姊妹项目 cloud-sync 写的 `cloud_instance_info` 表，只需 `SELECT`。**DSN 必须带 `parseTime=true&loc=UTC`**，否则时间会差一个时区。不配则 `d` 键提示未开启；配了但连不上，jmsd 启动失败。
- **`tunnel`**：开启后 jmsd 多监听一个 SSH 端口，只做 TCP 转发。无论 `allow` 怎么写，云元数据（`169.254.0.0/16`）和 jmsd 本机回环地址永远拒绝。`host_key` 是私钥，`chmod 600`。

### 4. 启动

```bash
./jmsd --config /etc/jmsd/config.yaml
```

配了 `inventory_db_dsn` 时启动要先加载云元数据，大约 10 秒后才开始监听。每个被拒绝的请求都会打一行 `jmsd: <方法> <路径> user=<用户> -> <状态码> <原因>`。

**jmsd 自己不做 TLS。** 跨公网时在前面挂 nginx / caddy 做 HTTPS，并把 `listen` 改成 `127.0.0.1:8391`，客户端写 `jmsd_addr: "https://..."`。连接票据是在这条连接上返回的。

## 构建与发布

```bash
cp Makefile.example Makefile                      # 填 S3 桶、CDN、JumpServer、jmsd 地址和 Google OAuth 客户端
cp scripts/install.example.sh scripts/install.sh  # 填同样的地址

make help                         # 全部目标
make build                        # 本机编译 jms / jmsd / jmsd-admin
make check                        # gofmt + vet + test，提交前跑
make jmsd-release VERSION=0.4.0   # jmsd，linux/amd64 静态链接，产物在 dist/
make release-dry  VERSION=0.4.0   # jms 四个平台，只编译到 dist/
make release      VERSION=0.4.0   # 同上并上传 CDN（会覆盖线上版本）
```

- **`Makefile` 和 `scripts/install.sh` 在 `.gitignore` 里**，内部地址和 Google client secret 不进仓库。`install.sh` 会被上传到公开可读的 CDN，里面还留着 `example.com` / `REPLACE-ME` 时发布会中止。
- **`VERSION` 必填**，格式 `X.Y.Z`。不要手写 `go build`：版本号靠 ldflags 注入，编出来的 `dev` 版本会让界面一直提示版本不一致。
- **先部署 jmsd，再发 jms。** 两者不兼容时（比如 0.4.0 换了登录方式）要连着发，中间不要停。
- 上传顺序是二进制、`version`、`install.sh`，最后 `manifest.json`（版本号 + sha256，自动更新以它为准）。上传后会逐个检查 CDN 返回 200。
- jmsd 不上 CDN，手动部署到机房那台机器。

## 当前限制

- 不支持 Windows（请用 WSL2）。
- 不支持「连接时手动输入密码」的账号（`@INPUT`），这类账号不会出现在选项里。
- 不支持 Chen（Web GUI）类资产。
- Redis 查询不跟随集群的 `MOVED` 重定向。
- 隧道只做 TCP，且 Core 里没有记录。
- 开启 `require_permitted_asset` 后，地址是域名而不是 IP 的资产无法通过隧道访问。

## 安全

- jmsd 的配置文件 `chmod 600`，不进版本库。
- Core 数据库账号只给 `SELECT`，外加 `assets_favoriteasset` 的 `INSERT` / `DELETE`。
- Redis 必须 `bind 127.0.0.1`。
- jmsd 只讲明文 HTTP，出内网前必须挂 TLS。
- 给 AI 用的 JumpServer 用户只授权只读账号，并配置命令过滤。

发现漏洞请私下报告给仓库 owner，不要开公开 issue。

## License

[MIT](LICENSE)
