#!/bin/bash
#
# jms 一键安装。发布位置来自 JMS_CDN_BASE，仓库里不写死任何一家的域名。
#
# scripts/release.sh 上传这个文件时会把 JMS_CDN_BASE 的真实值替换进默认值里，
# 所以 CDN 上那一份 curl 下来直接能跑；仓库里这一份指向 example.com，需要
# 自己 export JMS_CDN_BASE 再执行。
cdn_base="${JMS_CDN_BASE:-https://dist.example.com/jms}"

base_path='/usr/local/bin'
jms_path="$base_path/jms"

if [ ! -d "$base_path" ]; then
    mkdir -p "$base_path"
fi
rm -f "$jms_path"

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

if ! curl -fsSL -o "$jms_path" "$cdn_base/${target_dir}/jms"; then
    echo "下载失败，未安装。" >&2
    rm -f "$jms_path"
    exit 1
fi

chmod a+x "$jms_path"
curl -fsS "$cdn_base/version" -o ~/.jms_version 2>/dev/null || true

echo "安装完成。运行 jms 开始使用。"
echo "首次运行前请先写好配置文件（server / jmsd_addr 必填），见 jms.example.yaml。"
