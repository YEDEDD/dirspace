#!/usr/bin/env bash
# 卸载 dirspace。默认保留配置和 SQLite 历史数据。
set -euo pipefail

BIN_FILE="/usr/local/bin/dirspace"
UNIT_FILE="/etc/systemd/system/dirspace.service"
CONFIG_DIR="/etc/dirspace"
STATE_DIR="/var/lib/dirspace"

PURGE_DATA=0
PURGE_CONFIG=0
ASSUME_YES=0
STOP_SERVICE=1

usage() {
  cat <<'USAGE'
用法: sudo scripts/uninstall.sh [选项]

默认行为：停止并禁用服务，删除 systemd 单元和二进制；
          保留 /etc/dirspace 配置与 /var/lib/dirspace SQLite 历史。

  --purge-data     同时删除数据库、WAL、锁和本地状态目录（需确认）
  --purge-config   同时删除 /etc/dirspace 配置
  --yes            跳过 --purge-data 的交互确认
  --no-stop        不停止正在运行的服务（不推荐）
  -h, --help       显示帮助
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --purge-data) PURGE_DATA=1 ;;
    --purge-config) PURGE_CONFIG=1 ;;
    --yes) ASSUME_YES=1 ;;
    --no-stop) STOP_SERVICE=0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "错误: 请用 root 运行，例如: sudo scripts/uninstall.sh" >&2
  exit 1
fi

# 防御性检查：只允许操作固定系统路径。
[[ "$STATE_DIR" == "/var/lib/dirspace" ]] || { echo "拒绝操作非常规数据目录: $STATE_DIR" >&2; exit 1; }
[[ "$CONFIG_DIR" == "/etc/dirspace" ]] || { echo "拒绝操作非常规配置目录: $CONFIG_DIR" >&2; exit 1; }

if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  if [[ "$STOP_SERVICE" -eq 1 ]]; then
    systemctl stop dirspace.service 2>/dev/null || true
  fi
  systemctl disable dirspace.service 2>/dev/null || true
fi

rm -f "$UNIT_FILE"
if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  systemctl daemon-reload
fi
rm -f "$BIN_FILE"
rm -f /usr/share/doc/dirspace/README.md
rmdir /usr/share/doc/dirspace 2>/dev/null || true

if [[ "$PURGE_DATA" -eq 1 ]]; then
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    echo "将永久删除以下 SQLite 历史数据："
    echo "  ${STATE_DIR}/（数据库、WAL、锁、备份和损坏副本）"
    answer=""
    read -r -p "确认删除？输入 yes: " answer || true
    if [[ "$answer" != "yes" ]]; then
      echo "已取消删除数据。服务与二进制已卸载，数据保留。"
      exit 0
    fi
  fi
  rm -rf -- "$STATE_DIR"
  echo "已删除 SQLite 历史数据、锁和备份副本。"
else
  echo "已保留 SQLite 历史数据: ${STATE_DIR}/usage.db"
fi

if [[ "$PURGE_CONFIG" -eq 1 ]]; then
  rm -f "${CONFIG_DIR}/config.yaml"
  rmdir "$CONFIG_DIR" 2>/dev/null || true
  echo "已删除配置: ${CONFIG_DIR}"
else
  echo "已保留配置: ${CONFIG_DIR}/config.yaml"
fi

echo "卸载完成。"
