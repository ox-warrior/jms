#!/bin/bash
#
# jms 一键安装。三个地址都来自环境变量，仓库里不写死任何一家的域名。
#
# scripts/release.sh 上传这个文件时会把真实值替换进下面三个默认值里，所以 CDN
# 上那一份 curl 下来直接能跑；仓库里这一份全指向 example.com，自己跑需要先
# export 对应的变量。
#
# 一个文件而不是「公开版 + 内网版」两个：两份脚本必然漂移，而漂移的那一份是
# 没人 curl 的那份，坏了也没人知道。
set -u

cdn_base="${JMS_CDN_BASE:-https://dist.example.com/jms}"
# 写进初始配置文件的两个必填项。留空就不生成配置，用户自己照 jms.example.yaml 写。
jms_server="${JMS_SERVER:-}"
jms_jmsd_addr="${JMS_JMSD_ADDR:-}"

base_path='/usr/local/bin'
jms_path="$base_path/jms"

platform=$(uname -s | tr '[:upper:]' '[:lower:]')
machine_arch=$(uname -m)

case "$platform" in
    darwin)
        if [[ "$machine_arch" == "arm64" ]]; then
            target_dir="darwin"
        else
            target_dir="amd"
        fi
        ;;
    linux)
        if [[ "$machine_arch" == "x86_64" ]]; then
            target_dir="linux"
        else
            echo "jms 目前只提供 Linux amd64 (x86_64) 版本，$machine_arch 暂未发布。" >&2
            exit 1
        fi
        ;;
    *)
        echo "jms 目前只提供 macOS / Linux 版本，$platform 暂未发布。" >&2
        exit 1
        ;;
esac

# 装到哪都行，但不能就地覆盖那个路径。
#
# 之前是 `rm -f $jms_path` 然后 `curl -o $jms_path`，而升级时旧 jms 往往还在
# 跑（就是它提示你更新的）——文件还被引用着，rm 只解链接，curl 很可能拿回同一个
# inode。macOS 的内核按 inode 缓存已签名二进制的页哈希，于是新文件对上了旧
# 缓存，一执行就被判成「签名被篡改」，直接 SIGKILL：没有任何错误输出，只有
# shell 报一个 137。
#
# 下到临时文件再 mv 覆盖。mv 是原子 rename，一定落在新 inode 上，同时也让
# 「下载中途失败」不会留下一个装了一半的 jms。
tmp_path=$(mktemp "${TMPDIR:-/tmp}/jms.XXXXXX") || {
    echo "无法创建临时文件，未安装。" >&2
    exit 1
}
trap 'rm -f "$tmp_path"' EXIT

if ! curl -fsSL -o "$tmp_path" "$cdn_base/${target_dir}/jms"; then
    echo "下载失败，未安装。原来那个 jms 没有被动过。" >&2
    exit 1
fi

# 空文件或几 KB 的东西不是二进制，多半是 CDN 返回了一个错误页而 curl 没识别
# 出来。装上去的话，用户看到的是「jms: cannot execute binary file」，而真正
# 的原因（发布传坏了）无从得知。
size=$(wc -c < "$tmp_path" | tr -d ' ')
if [ "${size:-0}" -lt 1000000 ]; then
    echo "下载到的文件只有 $size 字节，不像 jms，未安装。" >&2
    echo "过一会儿重试；一直这样就是发布出了问题。" >&2
    exit 1
fi

# 0755，不是 chmod a+x：mktemp 建的文件是 0600，加个执行位就成了 0711 ——
# 别人能执行但读不了，而 /usr/local/bin 里的东西是给这台机器上所有人用的。
chmod 0755 "$tmp_path"

# 需要 sudo 就明说，而不是让 mv 吐一行 Permission denied 了事。
sudo_cmd=""
if [ ! -w "$base_path" ]; then
    if [ ! -d "$base_path" ]; then
        sudo_cmd="sudo"
    elif command -v sudo >/dev/null 2>&1; then
        echo "$base_path 需要管理员权限，下面会向你要密码。"
        sudo_cmd="sudo"
    else
        echo "$base_path 不可写，且这台机器上没有 sudo。未安装。" >&2
        exit 1
    fi
fi

$sudo_cmd mkdir -p "$base_path" || { echo "创建 $base_path 失败，未安装。" >&2; exit 1; }
$sudo_cmd mv -f "$tmp_path" "$jms_path" || { echo "写入 $jms_path 失败，未安装。" >&2; exit 1; }
trap - EXIT

# 这个文件只有老版本的 install.sh 还在读，留着是为了不让装了旧脚本的人看到
# 一个空文件；jms 自己判断要不要更新读的是 manifest.json。
curl -fsS "$cdn_base/version" -o ~/.jms_version 2>/dev/null || true

installed=$("$jms_path" version 2>/dev/null | head -1)
echo "安装完成${installed:+：$installed}。"

# 首次安装顺手写一份配置。
#
# server 和 jmsd_addr 是必填的，源码里没有内置默认值（任何一个默认地址都只对
# 一套部署成立），所以在这之前每个人装完都得先手写一遍这个文件才能启动——而
# 那两个地址是这次安装唯一知道它们的时刻。
#
# update_url 一起写进去，否则自动更新形同不存在：不配这一项 jms 就不检查、不
# 报错、也不写日志，于是发出去的版本没有任何人会收到提示。
#
# 已有的配置一个字都不动：里面可能有人手工改过的 log_level、cache_ttl 或者
# 指向另一套环境的地址，覆盖掉是在解决一个不存在的问题。
config_dir="$HOME/Library/Application Support/jms"
[ "$platform" = "linux" ] && config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/jms"
config_path="$config_dir/config.yaml"

if [ -f "$config_path" ]; then
    echo "配置文件已存在，未改动：$config_path"
elif [ -z "$jms_server" ] || [ -z "$jms_jmsd_addr" ]; then
    echo "首次运行前请先写好配置文件（server / jmsd_addr 必填），见 jms.example.yaml。"
else
    mkdir -p "$config_dir" && cat > "$config_path" <<CONFIG
# 由 install.sh 生成。可以随意修改，重新安装不会覆盖它。
server: "$jms_server"
jmsd_addr: "$jms_jmsd_addr"
update_url: "$cdn_base"
CONFIG
    if [ $? -eq 0 ]; then
        chmod 0600 "$config_path"
        echo "配置已写入：$config_path"
        echo "直接运行 jms 即可。"
    else
        echo "配置写入失败，请照 jms.example.yaml 手写一份：$config_path" >&2
    fi
fi
