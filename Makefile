# jms / jmsd 的统一入口。`make help` 列出所有目标。
#
# 两个二进制从同一个仓库出，但走的路完全不同：
#
#   jms   用户笔记本上的客户端，由 scripts/release.sh 交叉编译三个平台、上传
#         到 CDN，用户自动更新拿到。
#   jmsd  机房里的守护进程，手工部署一台。所以它不进 CDN——把一个持有 Core
#         凭据和只读库账号的服务放到公开分发里没有道理。
#
# VERSION 必须一路传到 ldflags。internal/version.Version 源码里躺的是 "dev"，
# 而 /v1/whoami 报的就是这个值、客户端拿它跟自己比——编出一个 dev 的 jmsd
# 会让每个人的界面顶上都挂一条「版本不一致」，指向错的方向。

MODULE  := github.com/leiyupeng/jumpserver-cli
VERSION ?= dev
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)

# 机房那台的架构。CGO_ENABLED=0 让它静态链接，扔到任何 glibc 版本的机器上都能
# 跑，不必对着目标机的 libc 试。
JMSD_GOOS   ?= linux
JMSD_GOARCH ?= amd64

.DEFAULT_GOAL := help
.PHONY: help build jms jmsd jmsd-admin jmsd-release release release-dry test test-race check fmt vet clean

help: ## 列出所有目标
	@grep -hE '^[a-z][a-zA-Z0-9_-]*:.*##' $(MAKEFILE_LIST) \
		| sed -E 's/:.*## /\t/' | expand -t 20
	@echo
	@echo "发版：      make jmsd-release VERSION=0.2.0   # 机房那台的 jmsd"
	@echo "            make release VERSION=0.2.0        # jms 上 CDN"
	@echo "本机开发：  make build                        # 三个都编到仓库根目录"

# ─── 本机开发 ────────────────────────────────────────────────────────────────

build: jms jmsd jmsd-admin ## 编译三个二进制到仓库根目录（本机架构）

jms: ## 只编 jms
	go build -ldflags "$(LDFLAGS)" -o jms ./cmd/jms

jmsd: ## 只编 jmsd
	go build -ldflags "$(LDFLAGS)" -o jmsd ./cmd/jmsd

jmsd-admin: ## 只编 jmsd-admin（一次性的角色/账号配置工具）
	go build -ldflags "$(LDFLAGS)" -o jmsd-admin ./cmd/jmsd-admin

# ─── 发版 ────────────────────────────────────────────────────────────────────

## 交叉编译要部到机房的 jmsd。产物 dist/jmsd-<goos>-<goarch>
##
## VERSION 是必填的，没有默认值：默认成 dev 就是把「忘了传版本号」变成一个
## 编得出来、跑得起来、只是报错版本的二进制，而那个错误要等到有人看界面顶上
## 那条提示才会发现。
jmsd-release: ## 交叉编译部署用的 jmsd（要 VERSION=x.y.z）
	@[ "$(VERSION)" != "dev" ] || { \
		echo "要带版本号：make jmsd-release VERSION=0.2.0" >&2; exit 1; }
	@mkdir -p dist
	GOOS=$(JMSD_GOOS) GOARCH=$(JMSD_GOARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LDFLAGS)" -o dist/jmsd-$(JMSD_GOOS)-$(JMSD_GOARCH) ./cmd/jmsd
	@chmod +x dist/jmsd-$(JMSD_GOOS)-$(JMSD_GOARCH)
	@echo
	@echo "dist/jmsd-$(JMSD_GOOS)-$(JMSD_GOARCH)  ($(VERSION))"
	@echo
	@echo "部署："
	@echo "  scp dist/jmsd-$(JMSD_GOOS)-$(JMSD_GOARCH) <主机>:/tmp/jmsd"
	@echo "  sudo systemctl stop jmsd"
	@echo "  sudo mv /tmp/jmsd /usr/local/bin/jmsd && sudo chmod +x /usr/local/bin/jmsd"
	@echo "  sudo systemctl start jmsd && sudo journalctl -u jmsd -n 20"
	@echo
	@echo "先部 jmsd 再发 jms：新 jms 连老 jmsd 拿不到云元数据，d 面板和"
	@echo "OWNER/INSTANCE TYPE/MODULE 三列会全空，而界面只会说「jms 需更新」。"

release: ## 交叉编译 jms 并上传 CDN（要 VERSION=x.y.z，会覆盖生产）
	@[ "$(VERSION)" != "dev" ] || { \
		echo "要带版本号：make release VERSION=0.2.0" >&2; exit 1; }
	./scripts/release.sh $(VERSION)

release-dry: ## 同 release 但只编到 dist/，不碰网络
	@[ "$(VERSION)" != "dev" ] || { \
		echo "要带版本号：make release-dry VERSION=0.2.0" >&2; exit 1; }
	./scripts/release.sh $(VERSION) --build-only

# ─── 检查 ────────────────────────────────────────────────────────────────────

check: fmt vet test ## gofmt + vet + test，提交前跑这个

test: ## 跑测试
	go test ./... -count=1

test-race: ## 带 race detector 跑（TUI 那两个包）
	go test ./internal/tui/ ./internal/jmsd/ -race -count=1

fmt: ## 报告没格式化的文件（不改动，改动请自己跑 gofmt -w）
	@out=$$(gofmt -l internal cmd 2>/dev/null); \
		[ -z "$$out" ] || { echo "以下文件需要 gofmt -w："; echo "$$out"; exit 1; }

vet: ## go vet
	go vet ./...

clean: ## 删掉二进制和 dist/
	rm -rf dist jms jmsd jmsd-admin
