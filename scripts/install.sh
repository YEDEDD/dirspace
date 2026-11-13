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
BINARY_OVERRIDE=""

usage() {
  cat <<'USAGE'
用法: sudo scripts/install.sh [--binary <文件>] [--no-start]

  --binary <文件>  指定 Linux 二进制；默认自动找 bin/dirspace 或
                   bin/dirspace-linux-<架构>
  --no-start       安装并 enable，但不立即启动服务
  -h, --help       显示帮助

也可用环境变量 DIRSPACE_BINARY=/path/to/dirspace。
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary)
      [[ $# -ge 2 ]] || { echo "错误: --binary 需要文件路径" >&2; exit 2; }
      BINARY_OVERRIDE="$2"
      shift 2
      ;;
    --no-start)
      START_SERVICE=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "未知参数: $1" >&2
      usage >&2
      exit 2
      ;;
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

DEFAULT_BINARY="${PROJECT_DIR}/bin/dirspace"
case "$(uname -m)" in
  x86_64|amd64) ARCH_BINARY="${PROJECT_DIR}/bin/dirspace-linux-amd64" ;;
  aarch64|arm64) ARCH_BINARY="${PROJECT_DIR}/bin/dirspace-linux-arm64" ;;
  *) ARCH_BINARY="" ;;
esac
if [[ ! -x "$DEFAULT_BINARY" && -n "$ARCH_BINARY" && -x "$ARCH_BINARY" ]]; then
  DEFAULT_BINARY="$ARCH_BINARY"
fi
BINARY="${BINARY_OVERRIDE:-${DIRSPACE_BINARY:-$DEFAULT_BINARY}}"
if [[ ! -x "$BINARY" ]]; then
  echo "未找到可执行文件: $BINARY"
  if command -v go >/dev/null 2>&1 && [[ -d "${PROJECT_DIR}/cmd/dirspace" ]]; then
    echo "尝试使用 Go 编译..."
    mkdir -p "${PROJECT_DIR}/bin"
    (cd "$PROJECT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/dirspace ./cmd/dirspace)
    BINARY="${PROJECT_DIR}/bin/dirspace"
  else
    echo "错误: 请用 --binary 指定 Linux 二进制，或使用 Release 压缩包。" >&2
    exit 1
  fi
fi
for asset in configs/config.yaml deploy/dirspace.service README.md; do
  if [[ ! -f "${PROJECT_DIR}/${asset}" ]]; then
    echo "错误: 缺少安装文件 ${PROJECT_DIR}/${asset}" >&2
    echo "请使用 Release 压缩包，或在仓库根目录执行 scripts/install.sh。" >&2
    exit 1
  fi
done
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

echo "安装 systemd 单元到 ${UNIT_FILE}"
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
