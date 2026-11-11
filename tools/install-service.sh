#!/bin/sh
# Register SpawnPoint once, or safely update its existing systemd installation.
set -eu

usage() {
  cat <<'EOF'
Usage: sudo sh tools/install-service.sh [--user service-user] [--source /path/to/spawnpoint] [--start]
  --user     Non-root service account, used only for the first installation.
             Defaults to SUDO_USER when invoked with sudo.
  --source   Use an existing Linux executable instead of rebuilding from source.
  --start    Start a newly registered or currently stopped service.
With no --source, build the current checkout automatically. If SpawnPoint is
already installed, update its executable and restart it if it was running.
Existing service settings, database, users and logs are not replaced.
EOF
}

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd -P)
service_user=$(printenv SUDO_USER 2>/dev/null || true)
source_exe="$repo_root/dist/linux64/spawnpoint"
explicit_source=0
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
      explicit_source=1
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
if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemctl is required (this installer targets systemd)." >&2
  exit 1
fi

unit=/etc/systemd/system/spawnpoint.service
installed=/opt/spawnpoint/spawnpoint
existing=0
if [ -e "$unit" ] || [ -L "$unit" ] || systemctl cat spawnpoint.service >/dev/null 2>&1; then
  existing=1
  # Never rewrite a different service, a symlinked unit, or a custom ExecStart.
  if [ ! -f "$unit" ] || [ -L "$unit" ] || \
     ! grep -Eq '^[[:space:]]*ExecStart=/opt/spawnpoint/spawnpoint[[:space:]]*$' "$unit"; then
    echo "Existing SpawnPoint service uses a nonstandard registration; refusing to overwrite it." >&2
    exit 1
  fi
  effective_exec=$(systemctl show -p ExecStart --value spawnpoint.service)
  case "$effective_exec" in
    *"path=/opt/spawnpoint/spawnpoint "*|*"path=/opt/spawnpoint/spawnpoint;"*) ;;
    *) echo "SpawnPoint ExecStart has been overridden; refusing to update a different executable." >&2; exit 1 ;;
  esac
fi

if [ "$existing" -eq 0 ]; then
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
fi

# Always compile the currently checked-out source by default. Building as the
# sudo caller avoids leaving root-owned Go artifacts in the developer's clone.
if [ "$explicit_source" -eq 0 ]; then
  echo "Building SpawnPoint from current source..."
  build_path="/usr/local/go/bin:$PATH"
  caller=$(printenv SUDO_USER 2>/dev/null || true)
  if [ -n "$caller" ] && [ "$caller" != root ]; then
    sudo -H -u "$caller" env PATH="$build_path" sh "$script_dir/dist.sh"
  else
    env PATH="$build_path" sh "$script_dir/dist.sh"
  fi
fi
if [ ! -f "$source_exe" ] || [ ! -x "$source_exe" ]; then
  echo "Missing Linux executable: $source_exe" >&2
  exit 1
fi

if [ "$existing" -eq 1 ]; then
  # Preserve the original installation layout; custom symlinked directories or
  # executables must not be silently replaced with a different target.
  if [ -L /opt/spawnpoint ] || [ -L "$installed" ]; then
    echo "Installed SpawnPoint path is a symlink; refusing to overwrite it." >&2
    exit 1
  fi
  # Do not interrupt a running service when the built binary is unchanged.
  if [ -f "$installed" ] && cmp -s "$source_exe" "$installed"; then
    echo "SpawnPoint is already up to date."
    if [ "$start_now" -eq 1 ] && ! systemctl is-active --quiet spawnpoint.service; then
      systemctl start spawnpoint.service
    fi
    exit 0
  fi

  # Stage the replacement and a rollback copy before touching the active job.
  if [ ! -d /opt/spawnpoint ]; then install -d -m 0755 /opt/spawnpoint; fi
  staged=$(mktemp /opt/spawnpoint/.spawnpoint-new.XXXXXX)
  backup=$(mktemp /opt/spawnpoint/.spawnpoint-old.XXXXXX)
  cleanup() { rm -f "$staged" "$backup"; }
  trap 'cleanup' EXIT
  trap 'exit 1' HUP INT TERM
  install -m 0755 "$source_exe" "$staged"
  had_previous=0
  if [ -f "$installed" ]; then
    cp -p "$installed" "$backup"
    had_previous=1
  fi
  was_active=0
  if systemctl is-active --quiet spawnpoint.service; then
    was_active=1
    systemctl stop spawnpoint.service
  fi

  if ! mv -f "$staged" "$installed"; then
    if [ "$was_active" -eq 1 ]; then systemctl start spawnpoint.service || true; fi
    echo "Failed to install the updated binary; existing executable retained." >&2
    exit 1
  fi
  if [ "$was_active" -eq 1 ] || [ "$start_now" -eq 1 ]; then
    if ! systemctl start spawnpoint.service || ! systemctl is-active --quiet spawnpoint.service; then
      echo "Updated SpawnPoint failed to start; restoring previous executable." >&2
      systemctl stop spawnpoint.service || true
      if [ "$had_previous" -eq 1 ]; then
        mv -f "$backup" "$installed"
        if [ "$was_active" -eq 1 ]; then
          systemctl start spawnpoint.service || echo "Rollback also failed; inspect systemctl status spawnpoint." >&2
        fi
      else
        rm -f "$installed"
      fi
      exit 1
    fi
  fi
  trap - EXIT HUP INT TERM
  cleanup
  echo "SpawnPoint updated. Existing service settings and data were preserved."
  if [ "$was_active" -eq 0 ] && [ "$start_now" -eq 0 ]; then
    echo "The service was stopped and remains stopped. Use --start to start it."
  fi
  exit 0
fi

# First installation only: create the service unit without replacing any
# previous database, logs or process registrations.
install -d -m 0755 /opt/spawnpoint
install -d -o "$service_user" -g "$group" -m 0750 /var/lib/spawnpoint /var/log/spawnpoint
install -m 0755 "$source_exe" "$installed"

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
