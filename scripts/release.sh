#!/bin/bash
# 交叉编译 jms 的全部已发布架构并上传到 S3。
#
# 目录约定沿用 mfmc 的老规矩（historical，不是什么好名字，但别的工具也在用）：
#   darwin  -> macOS arm64 (Apple Silicon)
#   amd     -> macOS x86_64 (Intel)
#   linux   -> Linux x86_64
#   linux-arm64 -> Linux arm64 (aarch64)
# 新增一个目标只需要在下面 TARGETS 里加一行 "GOOS GOARCH s3目录名"。
#
# 用法：
#   scripts/release.sh 1.2.3              # 交互确认后编译+上传
#   scripts/release.sh 1.2.3 -y           # 跳过确认，直接执行（CI 用）
#   scripts/release.sh 1.2.3 --build-only # 只编译到 dist/（含 manifest.json），不上传
#
# 版本号是必填参数，不再从源码里读：internal/version.Version 现在是 ldflags 注入的
# var，源码里躺着的是 "dev"。省掉这个参数就等于发一个自称 dev 的正式包。
#
# 发布位置来自环境变量，仓库里不写死任何一家的桶名和域名：
#   JMS_S3_BUCKET   上传目标，默认 s3://example-dist/jms
#   JMS_CDN_BASE    对应的公开地址，默认 https://dist.example.com/jms
# 两者必须指向同一份内容 —— 一个上传、一个用来事后验证和写进客户端的 update_url。

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bucket="${JMS_S3_BUCKET:-s3://example-dist/jms}"
cdn_base="${JMS_CDN_BASE:-https://dist.example.com/jms}"
# 上传哪一份 install.sh。
#
# install.sh 里的三个地址是填死的真实值，它在 .gitignore 里、不进 git；模板
# install.example.sh 全指向 example.com。优先用前者，因为 CDN 上的脚本必须自带
# 正确地址——不能要求 curl | bash 的人先 export 一个他还不知道的环境变量。
#
# 没有它时退回模板，并允许用环境变量补地址（CI 或者临时发布用）。
if [ -f "$repo_root/scripts/install.sh" ]; then
    install_src="$repo_root/scripts/install.sh"
else
    install_src="$repo_root/scripts/install.example.sh"
fi
jms_server="${JMS_SERVER:-}"
jms_jmsd_addr="${JMS_JMSD_ADDR:-}"
dist_dir="$repo_root/dist"

TARGETS=(
    "darwin arm64 darwin"
    "darwin amd64 amd"
    "linux  amd64 linux"
    "linux  arm64 linux-arm64"
)

assume_yes=false
build_only=false
version=""
for arg in "$@"; do
    case "$arg" in
        -y|--yes) assume_yes=true ;;
        --build-only) build_only=true ;;
        -*)
            echo "未知参数：$arg" >&2
            exit 1
            ;;
        *)
            [ -z "$version" ] || { echo "版本号只能给一个：已有 ${version}，又来了 $arg" >&2; exit 1; }
            version="$arg"
            ;;
    esac
done

# 拿不到版本号就停，绝不猜一个：manifest.json 里的版本号是客户端判断「要不要更新」
# 的唯一依据，写错一次的代价是所有人要么收不到更新，要么更新完还一直被提示。
if [ -z "$version" ]; then
    echo "用法：scripts/release.sh <版本号> [-y] [--build-only]" >&2
    echo "例：  scripts/release.sh 1.2.3" >&2
    exit 1
fi
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    # 客户端用 semver 排序比大小，不是字符串相等；带 v 前缀或 -rc 后缀的都解析不了，
    # 会被当成「读不懂，那就不提示更新」，静默到没人发现。
    echo "版本号必须是 X.Y.Z（不带 v 前缀）：$version" >&2
    exit 1
fi

echo "版本：$version"
echo "目标："
for t in "${TARGETS[@]}"; do
    read -r goos goarch s3dir <<< "$t"
    echo "  - $goos/$goarch -> $bucket/$s3dir/jms"
done
$build_only || echo "  - $bucket/version"
$build_only || echo "  - $bucket/install.sh（来自 ${install_src#$repo_root/}）"
$build_only || echo "  - $bucket/manifest.json（版本号 + 各目标 sha256，最后上传）"

if ! $build_only && ! $assume_yes; then
    read -r -p "确认执行？[y/N] " reply
    [[ "$reply" =~ ^[Yy]$ ]] || { echo "已取消。"; exit 1; }
fi

rm -rf "$dist_dir"
mkdir -p "$dist_dir"

for t in "${TARGETS[@]}"; do
    read -r goos goarch s3dir <<< "$t"
    out="$dist_dir/$s3dir/jms"
    mkdir -p "$(dirname "$out")"
    echo "编译 $goos/$goarch -> $out"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build \
        -ldflags "-X github.com/leiyupeng/jumpserver-cli/internal/version.Version=$version \
                  -X github.com/leiyupeng/jumpserver-cli/internal/cli.googleClientID=${JMS_GOOGLE_CLIENT_ID:-} \
                  -X github.com/leiyupeng/jumpserver-cli/internal/cli.googleClientSecret=${JMS_GOOGLE_CLIENT_SECRET:-}" \
        -o "$out" ./cmd/jms
    chmod a+x "$out"
done

# 准备要上传的 install.sh：内网那份原样拿来（地址已填死），公开模板则把环境变量
# 里的地址替换进默认值。
sed -e "s|\${JMS_CDN_BASE:-https://dist.example.com/jms}|\${JMS_CDN_BASE:-$cdn_base}|" \
    -e "s|\${JMS_SERVER:-}|\${JMS_SERVER:-$jms_server}|" \
    -e "s|\${JMS_JMSD_ADDR:-}|\${JMS_JMSD_ADDR:-$jms_jmsd_addr}|" \
    "$install_src" > "$dist_dir/install.sh"

# 三个地址都必须是真的。这些检查存在的理由各不相同：
#
#   example.com   传上去的话，curl | bash 的人会去一个不存在的域名下载
#   REPLACE-ME    从模板拷出来忘了填，表现是「装完 jms 连不上」，跟服务挂了一样
#   缺 server     表现是「每个人装完都要手写配置」，而那正是这套要免掉的事
#
# 都在上传前，所以坏的那份永远到不了 CDN。
for bad in "dist.example.com" "jumpserver.example.com" "jmsd.example.com" "REPLACE-ME"; do
    grep -q "$bad" "$dist_dir/install.sh" && {
        echo "install.sh 里还留着占位符 ${bad}，中止上传。" >&2
        echo "填好 $install_src 里的地址（或 export JMS_SERVER / JMS_JMSD_ADDR）再发。" >&2
        exit 1
    }
done
grep -q "$cdn_base" "$dist_dir/install.sh" || {
    echo "install.sh 里的 CDN 地址不是 ${cdn_base}，中止上传。" >&2
    exit 1
}
grep -qE '^jms_server="\$\{JMS_SERVER:-[^}]+\}"' "$dist_dir/install.sh" || {
    echo "install.sh 里没有 server 地址，装完的人仍要手写配置。中止上传。" >&2
    exit 1
}
grep -qE '^jms_jmsd_addr="\$\{JMS_JMSD_ADDR:-[^}]+\}"' "$dist_dir/install.sh" || {
    echo "install.sh 里没有 jmsd 地址，装完的人仍要手写配置。中止上传。" >&2
    exit 1
}

# manifest.json 是 jms 启动时自动更新读的东西：版本号和每个目标的 sha256 必须来自
# 同一个文件，分两次拿会出现两边描述不同构建的窗口。客户端拿不到 sha256 就拒绝更新，
# 所以这个文件缺了不是「降级成不校验」，是「不更新」。
#
# 纯文本的 version 文件保持原样不动 —— 已经装在各人机器上的 install.sh 还在 curl 它，
# 改成 JSON 会让它往 ~/.jms_version 里写一坨。
sha256_of() {
    if command -v shasum > /dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    else
        sha256sum "$1" | cut -d' ' -f1
    fi
}

{
    printf '{\n  "version": "%s",\n  "targets": {\n' "$version"
    first=true
    for t in "${TARGETS[@]}"; do
        read -r goos goarch s3dir <<< "$t"
        $first || printf ',\n'
        first=false
        printf '    "%s": { "sha256": "%s" }' "$s3dir" "$(sha256_of "$dist_dir/$s3dir/jms")"
    done
    printf '\n  }\n}\n'
} > "$dist_dir/manifest.json"
echo "manifest：$dist_dir/manifest.json"

# printf，不是 echo -n：某些 shell 的 echo 不吃 -n，于是 CDN 上的 version 变成了
# 字面量 "-n 0.1.2"（真发生过，线上挂了一整个版本周期）。写在这里而不是上传那一段，
# 是为了让 --build-only 的产物和真上传的东西逐字节一致，能拿来核对。
printf '%s' "$version" > "$dist_dir/version"

if $build_only; then
    echo "只编译，未上传。产物在 $dist_dir/"
    exit 0
fi

for t in "${TARGETS[@]}"; do
    read -r goos goarch s3dir <<< "$t"
    aws s3 cp "$dist_dir/$s3dir/jms" "$bucket/$s3dir/jms"
done

aws s3 cp "$dist_dir/version" "$bucket/version"
aws s3 cp "$dist_dir/install.sh" "$bucket/install.sh"

# manifest 最后上传。它是客户端唯一的触发条件，先于二进制上去的话，已经在跑的 jms
# 会拿着新版本号去下还没替换完的旧文件，sha256 对不上，更新白失败一次。
aws s3 cp "$dist_dir/manifest.json" "$bucket/manifest.json"

echo
echo "上传完成，逐个验证 CDN："
for t in "${TARGETS[@]}"; do
    read -r goos goarch s3dir <<< "$t"
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$cdn_base/$s3dir/jms")
    echo "  $s3dir/jms -> HTTP $code"
done
for f in install.sh version manifest.json; do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$cdn_base/$f")
    echo "  $f -> HTTP $code"
done
echo
echo "CDN 上的 manifest（客户端就读这个）："
curl -sS "$cdn_base/manifest.json"
