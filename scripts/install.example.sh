#!/bin/bash
#
# jms 一键安装（模板）。三个地址都指向 example.com，仓库里不写死任何一家的域名。
#
# 实际发布用的是 scripts/install.sh —— 从这个文件拷一份、填上真实地址：
#
#     cp scripts/install.example.sh scripts/install.sh
#
# 那个文件在 .gitignore 里（和 Makefile 一个规矩）。release.sh 优先上传它，没有
# 就退回这一份，并允许用 JMS_SERVER / JMS_JMSD_ADDR / JMS_CDN_BASE 补地址。
#
# 为什么不是「一个文件靠 sed 替换」：替换出来的成品要上传到 CDN，而 CDN 是公开
# 可读的（curl 一下就有）。jmsd 的地址是内网的，落在那儿等于公开——所以带真实
# 地址的那一份从头到尾不进 git，只在发布机上存在。
set -u

cdn_base="${JMS_CDN_BASE:-https://dist.example.com/jms}"
# 写进初始配置文件的两个必填项。留空就不生成配置，用户自己照 jms.example.yaml 写。
jms_server="${JMS_SERVER:-}"
jms_jmsd_addr="${JMS_JMSD_ADDR:-}"

base_path='/usr/local/bin'
jms_path="$base_path/jms"

# 装给谁。`curl … | sudo bash` 时脚本是 root，但 jms 是给敲命令的那个人用的：
# 二进制和配置都要归他，否则配置他读不了，强制更新也换不掉二进制。
run_user="${SUDO_USER:-$(id -un)}"
user_home=$(eval echo "~$run_user")
# 真正的二进制放在用户自己的目录里，/usr/local/bin/jms 只是指过去的软链接：
# jms 自更新时会解析软链接，替换的是这一份，不需要 sudo。
real_dir="$user_home/.jms/bin"
real_path="$real_dir/jms"
give_to_user() {
    if [ "$(id -u)" = 0 ] && [ "$run_user" != root ]; then
        chown -R "$run_user" "$@"
    fi
}

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
        case "$machine_arch" in
            x86_64) target_dir="linux" ;;
            aarch64|arm64) target_dir="linux-arm64" ;;
            *)
                echo "jms 目前只提供 Linux amd64 / arm64 版本，$machine_arch 暂未发布。" >&2
                exit 1
                ;;
        esac
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

mkdir -p "$real_dir" && mv -f "$tmp_path" "$real_path" || {
    echo "写入 $real_path 失败，未安装。" >&2
    exit 1
}
give_to_user "$user_home/.jms"

# 需要 sudo 就明说，而不是让 ln 吐一行 Permission denied 了事。只有建软链接
# 这一步可能要 sudo，而且只在第一次装、或者从老的装法迁过来时。
sudo_cmd=""
if [ ! -w "$base_path" ]; then
    if [ ! -d "$base_path" ]; then
        sudo_cmd="sudo"
    elif command -v sudo >/dev/null 2>&1; then
        echo "$base_path 需要管理员权限，下面会向你要密码。"
        sudo_cmd="sudo"
    else
        echo "$base_path 不可写，且这台机器上没有 sudo。jms 已装到 $real_path，请把 $real_dir 加进 PATH。" >&2
        exit 1
    fi
fi
if [ "$(readlink "$jms_path" 2>/dev/null)" != "$real_path" ]; then
    $sudo_cmd mkdir -p "$base_path" &&
        $sudo_cmd ln -sfn "$real_path" "$jms_path" || {
        echo "在 $jms_path 建软链接失败。jms 已装到 $real_path，请把 $real_dir 加进 PATH。" >&2
        exit 1
    }
fi
trap - EXIT

# 这个文件只有老版本的 install.sh 还在读，留着是为了不让装了旧脚本的人看到
# 一个空文件；jms 自己判断要不要更新读的是 manifest.json。
curl -fsS "$cdn_base/version" -o "$user_home/.jms_version" 2>/dev/null && give_to_user "$user_home/.jms_version" || true

installed=$("$real_path" version 2>/dev/null | head -1)
echo "安装完成${installed:+：$installed}。"

# 写配置。
#
# server 和 jmsd_addr 是必填的，源码里没有内置默认值（任何一个默认地址都只对
# 一套部署成立），所以在这之前每个人装完都得先手写一遍这个文件才能启动——而
# 这次安装正是唯一知道这些地址的时刻。
#
# update_url 一起写，否则自动更新形同不存在：不配这一项 jms 就不检查、不报错、
# 也不写日志，于是发出去的版本没有任何人会收到提示。
#
# 三项都覆盖，包括已经存在的配置。地址是会变的——jmsd 换台机器、CDN 换个域名
# ——而这个脚本是唯一能把新地址送到每个人手上的东西。不覆盖就等于每次变更都要
# 挨个通知人手动改，而那些改不动的人的表现是「jms 连不上」，跟服务挂了一样。
#
# 用户自己加的其他项（log_level、cache_ttl、k9s_path…）逐行保留：那些是偏好，
# 不是部署事实，没有理由被一次安装重置。
config_dir="$user_home/Library/Application Support/jms"
[ "$platform" = "linux" ] && config_dir="${XDG_CONFIG_HOME:-$user_home/.config}/jms"
config_path="$config_dir/config.yaml"

if [ -z "$jms_server" ] || [ -z "$jms_jmsd_addr" ]; then
    # 公开仓库那一份走这条路：没有地址可写，不留一个半截的配置。
    [ -f "$config_path" ] || \
        echo "首次运行前请先写好配置文件（server / jmsd_addr 必填），见 jms.example.yaml。"
else
    mkdir -p "$config_dir" || {
        echo "无法创建 ${config_dir}，请照 jms.example.yaml 手写一份配置。" >&2
        exit 1
    }
    tmp_config=$(mktemp "${TMPDIR:-/tmp}/jms-config.XXXXXX") || {
        echo "无法创建临时文件，配置未更新。" >&2
        exit 1
    }
    {
        echo "# server / jmsd_addr / update_url 由 install.sh 写入，重装会覆盖成最新地址。"
        echo "# 其他项是你自己的，重装保留。"
        echo "server: \"$jms_server\""
        echo "jmsd_addr: \"$jms_jmsd_addr\""
        echo "update_url: \"$cdn_base\""
        # 逐行搬回用户自己加的项。grep 的是行首的 key，所以注释和缩进（属于某个
        # 块的子项，比如将来的 k8s:）都不会被误当成顶层项搬过来。
        if [ -f "$config_path" ]; then
            grep -vE '^[[:space:]]*(#|$)' "$config_path" 2>/dev/null \
                | grep -vE '^(server|jmsd_addr|update_url):' || true
        fi
    } > "$tmp_config"

    if mv -f "$tmp_config" "$config_path"; then
        chmod 0600 "$config_path"
        give_to_user "$config_dir"
        echo "配置已写入：$config_path"
    else
        rm -f "$tmp_config"
        echo "配置写入失败，请照 jms.example.yaml 手写一份：$config_path" >&2
    fi
fi
