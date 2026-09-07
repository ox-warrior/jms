#!/bin/bash
# 交叉编译 jms 的全部已发布架构并上传到 S3。
#
# 目录约定沿用 mfmc 的老规矩（historical，不是什么好名字，但别的工具也在用）：
#   darwin  -> macOS arm64 (Apple Silicon)
#   amd     -> macOS x86_64 (Intel)
#   linux   -> Linux x86_64
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
# 这两个被替换进 install.sh，让它首次安装时写出一份能直接用的配置。留空就是
# 不写配置，用户装完仍需自己照 jms.example.yaml 手写一份。
jms_server="${JMS_SERVER:-}"
jms_jmsd_addr="${JMS_JMSD_ADDR:-}"
dist_dir="$repo_root/dist"

TARGETS=(
    "darwin arm64 darwin"
    "darwin amd64 amd"
    "linux  amd64 linux"
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
            [ -z "$version" ] || { echo "版本号只能给一个：已有 $version，又来了 $arg" >&2; exit 1; }
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
$build_only || echo "  - $bucket/install.sh（同步 scripts/install.sh）"
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
        -ldflags "-X github.com/leiyupeng/jumpserver-cli/internal/version.Version=$version" \
        -o "$out" ./cmd/jms
    chmod a+x "$out"
done

# 把真实地址替换进 install.sh 的默认值再上传。仓库里那一份全指向 example.com
# （公开仓库不写死任何一家的域名），而 CDN 上这一份是给人 curl | bash 的，必须自带
# 正确地址 —— 不能要求用户先 export 一个他还不知道的环境变量。
sed -e "s|\${JMS_CDN_BASE:-https://dist.example.com/jms}|\${JMS_CDN_BASE:-$cdn_base}|" \
    -e "s|\${JMS_SERVER:-}|\${JMS_SERVER:-$jms_server}|" \
    -e "s|\${JMS_JMSD_ADDR:-}|\${JMS_JMSD_ADDR:-$jms_jmsd_addr}|" \
    "$repo_root/scripts/install.sh" > "$dist_dir/install.sh"
grep -q "$cdn_base" "$dist_dir/install.sh" || {
    echo "install.sh 里的 CDN 地址没替换成功，中止上传。" >&2
    exit 1
}
# server/jmsd_addr 是可选的，但设了就必须替换成功 —— 静默传上去一个不写配置的
# install.sh，表现是「每个人装完都要手写配置」，而那正是设这两个变量要免掉的事。
for pair in "JMS_SERVER:$jms_server" "JMS_JMSD_ADDR:$jms_jmsd_addr"; do
    value="${pair#*:}"
    [ -z "$value" ] && continue
    grep -q "$value" "$dist_dir/install.sh" || {
        echo "install.sh 里的 ${pair%%:*} 没替换成功，中止上传。" >&2
        exit 1
    }
done

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
