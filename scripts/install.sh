#!/usr/bin/env bash
# 安装 dirspace：二进制、配置、systemd 单元和本地数据目录。
# 默认保留已有配置与 SQLite 历史数据。
set -euo pipefail

BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/dirspace"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
STATE_DIR="/var/lib/dirspace"
UNIT_FILE="/etc/systemd/system/dirspace.service"
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
START_SERVICE=1

usage() {
  cat <<'USAGE'
用法: sudo scripts/install.sh [--no-start]

  --no-start   安装并 enable，但不立即启动服务
  -h, --help   显示帮助

可通过环境变量覆盖：
  DIRSPACE_BINARY=/path/to/dirspace   使用已编译好的 Linux 二进制
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --no-start) START_SERVICE=0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "错误: dirspace 生产安装仅支持 Linux；当前是 $(uname -s)。" >&2
  echo "开发机可用 'make build' 编译本机版本。" >&2
  exit 1
fi
if [[ "${EUID}" -ne 0 ]]; then
  echo "错误: 请用 root 运行，例如: sudo scripts/install.sh" >&2
  exit 1
fi

BINARY="${DIRSPACE_BINARY:-${PROJECT_DIR}/bin/dirspace}"
if [[ ! -x "$BINARY" ]]; then
  echo "未找到可执行文件: $BINARY"
  if command -v go >/dev/null 2>&1; then
    echo "尝试使用 Go 编译..."
    mkdir -p "${PROJECT_DIR}/bin"
    (cd "$PROJECT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/dirspace ./cmd/dirspace)
    BINARY="${PROJECT_DIR}/bin/dirspace"
  else
    echo "错误: 请先在构建机执行 'make build-linux'，再用 DIRSPACE_BINARY 指定文件。" >&2
    exit 1
  fi
fi
if ! file "$BINARY" 2>/dev/null | grep -q 'ELF'; then
  echo "警告: 未检测到 ELF 二进制，请确认这是 Linux 版本。" >&2
fi

echo "安装二进制到 ${BIN_DIR}/dirspace"
install -D -m 0755 "$BINARY" "${BIN_DIR}/dirspace"

mkdir -p "$CONFIG_DIR"
if [[ -f "$CONFIG_FILE" ]]; then
  echo "保留已有配置: $CONFIG_FILE"
else
  install -m 0640 "${PROJECT_DIR}/configs/config.yaml" "$CONFIG_FILE"
  echo "已安装默认配置: $CONFIG_FILE"
  echo "请确认 scan.root_path 指向真实 NFS 挂载，例如 /mnt 或 /data。"
fi
chmod 0750 "$CONFIG_DIR"

install -d -m 0700 "$STATE_DIR"
echo "数据目录: $STATE_DIR（已有数据库不会被覆盖）"

echo "安装 systemd 单元"
install -D -m 0644 "${PROJECT_DIR}/deploy/dirspace.service" "$UNIT_FILE"
install -D -m 0644 "${PROJECT_DIR}/README.md" /usr/share/doc/dirspace/README.md

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl daemon-reload
  systemctl enable dirspace.service
  if [[ "$START_SERVICE" -eq 1 ]]; then
    systemctl restart dirspace.service
    echo "服务已启动。查看日志: journalctl -u dirspace -f"
  else
    echo "服务已 enable，未启动。手动启动: systemctl start dirspace"
  fi
else
  echo "警告: 当前环境没有运行 systemd，已复制单元但未 enable/启动。" >&2
  echo "单元文件: $UNIT_FILE" >&2
fi

echo
echo "安装完成。验证:"
echo "  dirspace --config ${CONFIG_FILE} --path /mnt status"
echo "  dirspace --config ${CONFIG_FILE} --path /mnt quick"
echo "  dirspace --config ${CONFIG_FILE} --path /mnt top 10"
