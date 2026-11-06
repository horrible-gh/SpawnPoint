#!/bin/sh
# Register SpawnPoint as a systemd service. Does not start it unless --start is supplied.
set -eu

usage() {
  cat <<'EOF'
Usage: sudo sh tools/install-service.sh [--user service-user] [--source /path/to/spawnpoint] [--start]
  --user     Non-root account that will run SpawnPoint and its child commands.
             Defaults to SUDO_USER when invoked through sudo.
  --source   Linux executable (default: dist/linux64/spawnpoint).
  --start    Also start the registered service immediately.
This script enables the service on boot but does not start it by default.
EOF
}

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd -P)
service_user=$(printenv SUDO_USER 2>/dev/null || true)
source_exe="$repo_root/dist/linux64/spawnpoint"
start_now=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --user)
      [ "$#" -ge 2 ] || { usage >&2; exit 2; }
      service_user="$2"
      shift 2
      ;;
    --source)
      [ "$#" -ge 2 ] || { usage >&2; exit 2; }
      source_exe="$2"
      shift 2
      ;;
    --start)
      start_now=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root using sudo. The service itself will not run as root." >&2
  exit 1
fi
case "$service_user" in
  ""|root)
    echo "Specify an existing non-root service user with --user (or use sudo from that account)." >&2
    exit 1
    ;;
  *[!a-zA-Z0-9_.-]*)
    echo "Service user contains unsupported characters." >&2
    exit 1
    ;;
esac

if ! id "$service_user" >/dev/null 2>&1; then
  echo "Service user does not exist: $service_user" >&2
  exit 1
fi
group=$(id -gn "$service_user")
case "$group" in
  ""|*[!a-zA-Z0-9_.-]*)
    echo "Service primary group contains unsupported characters." >&2
    exit 1
    ;;
esac

if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemctl is required (this installer targets systemd)." >&2
  exit 1
fi
if [ ! -f "$source_exe" ] || [ ! -x "$source_exe" ]; then
  echo "Missing Linux executable: $source_exe" >&2
  echo "Build it first: sh tools/dist.sh" >&2
  exit 1
fi

unit=/etc/systemd/system/spawnpoint.service
if [ -e "$unit" ] || systemctl cat spawnpoint.service >/dev/null 2>&1; then
  echo "SpawnPoint service already exists; refusing to overwrite its registration." >&2
  exit 1
fi

install -d -m 0755 /opt/spawnpoint
install -d -o "$service_user" -g "$group" -m 0750 /var/lib/spawnpoint /var/log/spawnpoint
install -m 0755 "$source_exe" /opt/spawnpoint/spawnpoint

cat > "$unit" <<EOF
[Unit]
Description=SpawnPoint process manager
After=network.target

[Service]
Type=simple
User=$service_user
Group=$group
WorkingDirectory=/var/lib/spawnpoint
ExecStart=/opt/spawnpoint/spawnpoint
Environment=SPAWNPOINT_HOST=127.0.0.1
Environment=SPAWNPOINT_PORT=7527
Environment=SPAWNPOINT_DB_PATH=/var/lib/spawnpoint/spawnpoint.db
Environment=SPAWNPOINT_LOG_DIR=/var/log/spawnpoint
Environment=SPAWNPOINT_KILL_CHILDREN_ON_EXIT=1
UMask=0077
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=5
TimeoutStopSec=50
KillMode=control-group

[Install]
WantedBy=multi-user.target
EOF

chmod 0644 "$unit"
systemctl daemon-reload
systemctl enable spawnpoint.service
if [ "$start_now" -eq 1 ]; then
  systemctl start spawnpoint.service
fi
printf 'SpawnPoint registered as spawnpoint.service (user: %s).\n' "$service_user"
if [ "$start_now" -eq 0 ]; then
  printf 'Start when ready: sudo systemctl start spawnpoint\n'
fi
printf 'Status: systemctl status spawnpoint\n'
