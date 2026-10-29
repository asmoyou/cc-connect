# Asmo fork builds

我们 fork 的维护和打包分支是 `main`。通过检查后，GitHub Actions 自动更新 [asmo-latest 预览版](https://github.com/asmoyou/cc-connect/releases/tag/asmo-latest)。这是包含消息队列卡片改进的 fork 版本。

目前提供两个预编译版本，无需拉取源码或安装 Go：

| 系统 | 架构 | Release 文件 |
| --- | --- | --- |
| Ubuntu 等 Linux 系统 | ARM64 / aarch64 | `cc-connect-linux-arm64.tar.gz` |
| macOS | Apple Silicon / arm64 | `cc-connect-darwin-arm64.tar.gz` |

Linux ARM64 成品会在原生 Ubuntu ARM runner 上解压、校验并运行 `--version` 和 `--help` 后发布。macOS 成品通过交叉编译生成。

## Ubuntu ARM64 安装或更新

运行 `uname -m`，应显示 `aarch64`。停止正在运行的 cc-connect 服务后执行：

```bash
set -e
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
curl --fail --location --retry 3 \
  https://github.com/asmoyou/cc-connect/releases/download/asmo-latest/cc-connect-linux-arm64.tar.gz \
  --output "$work_dir/cc-connect.tar.gz"
tar -xzf "$work_dir/cc-connect.tar.gz" -C "$work_dir"
(cd "$work_dir" && sha256sum --check SHA256SUMS)
mkdir -p "$HOME/.local/bin"
install -m 755 "$work_dir/cc-connect" "$HOME/.local/bin/cc-connect"
"$HOME/.local/bin/cc-connect" --version
```

然后用原有配置和启动方式重启服务。安装路径为 `~/.local/bin/cc-connect`；已有服务使用其他路径时，将上面的目标路径改为服务实际使用的位置。安装完成后临时下载与解压文件会自动清理，不备份旧程序。配置和会话记录保留。

macOS 使用同一 Release 下的 `darwin-arm64` 压缩包，解压后用 `shasum -a 256 -c SHA256SUMS` 校验，再安装可执行文件。

官方 `npm install -g cc-connect` 和 `cc-connect update` 仍从官方仓库获取版本，不会自动安装这个 fork 的预览版。更新这个预览版时，重新下载对应的 Release 成品即可。

Actions 的 Artifacts 也保留构建成品 14 天；日常下载安装使用上面的 Releases 链接。`asmo-latest` 会随通过检查的 `main` 更新，具体源码提交和构建记录在 Release 说明中列出。
