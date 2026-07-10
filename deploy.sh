#!/usr/bin/env bash
# deploy.sh — provision an Ubicloud VM and run r2proxy on it as a systemd service.
#
#   ./deploy.sh up        provision VM + firewall, build, deploy, start   (idempotent)
#   ./deploy.sh push      rebuild the binary and restart on the existing VM
#   ./deploy.sh info      print connection details + admin token
#   ./deploy.sh logs      tail the service logs
#   ./deploy.sh ssh       open a shell on the VM
#   ./deploy.sh destroy   tear the VM (and its subnet/firewall) down
#
# Config comes from ./deploy.env (see deploy.env.example). Redeploy to a fresh
# server by changing DEPLOY_NAME / DEPLOY_LOCATION and running `up` again.
set -euo pipefail

cd "$(dirname "$0")"
[ -f deploy.env ] && set -a && . ./deploy.env && set +a

: "${UBI_TOKEN:?set UBI_TOKEN (in deploy.env)}"
export UBI_TOKEN
LOCATION="${DEPLOY_LOCATION:-eu-central-h1}"
NAME="${DEPLOY_NAME:-r2proxy}"
SIZE="${DEPLOY_SIZE:-standard-2}"
FW="${NAME}-fw"
NET="${NAME}-net"
SSH_KEY="${DEPLOY_SSH_KEY:-$HOME/.ssh/id_ed25519}"
REMOTE_DIR="/home/ubi/r2proxy"

say(){ printf '\033[1;33m▶ %s\033[0m\n' "$*"; }
ubivm(){ ubi vm "$LOCATION/$NAME" "$@"; }

build(){
  say "building static linux/amd64 binary"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/r2proxy .
  ls -lh dist/r2proxy | awk '{print "  "$5" dist/r2proxy"}'
}

ensure_firewall(){
  if ! ubi fw list 2>/dev/null | grep -qw "$FW"; then
    say "creating firewall $FW"
    ubi fw "$LOCATION/$FW" create -d "r2proxy" >/dev/null
  fi
  # SSH + admin console: optionally lock to your current public IP.
  local admin_cidr="0.0.0.0/0"
  if [ "${LOCK_ADMIN_TO_MY_IP:-false}" = "true" ]; then
    local myip; myip="$(curl -fsS https://api.ipify.org 2>/dev/null || true)"
    [ -n "$myip" ] && admin_cidr="$myip/32" && say "locking SSH+admin to $admin_cidr"
  fi
  add_rule(){ ubi fw "$LOCATION/$FW" add-rule -s "$1" -e "$1" "$2" >/dev/null 2>&1 || true; }
  add_rule 22   "$admin_cidr"
  add_rule 8081 "$admin_cidr"
  add_rule 8080 "0.0.0.0/0"   # data-plane: open, authenticated by SigV4
  add_rule 8080 "::/0"
}

ensure_subnet(){
  if ! ubi ps list 2>/dev/null | grep -qw "$NET"; then
    say "creating subnet $NET on firewall $FW"
    ubi ps "$LOCATION/$NET" create -f "$FW" >/dev/null
  fi
}

vm_exists(){ ubi vm list 2>/dev/null | grep -qw "$NAME"; }

ensure_vm(){
  [ -f "${SSH_KEY}.pub" ] || { say "generating ssh key $SSH_KEY"; ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" >/dev/null; }
  if vm_exists; then say "VM $NAME already exists"; return; fi
  say "creating VM $NAME ($SIZE) in $LOCATION"
  ubivm create -s "$SIZE" -p "$NET" "$(cat "${SSH_KEY}.pub")" >/dev/null
  say "waiting for VM to boot + accept SSH"
  for i in $(seq 1 60); do
    if ubivm ssh -- true 2>/dev/null; then say "SSH is up"; return; fi
    sleep 5
  done
  echo "timed out waiting for SSH" >&2; exit 1
}

deploy_binary(){
  say "uploading binary + unit to VM"
  ubivm ssh -- "mkdir -p $REMOTE_DIR"
  # Upload to a temp name — the live binary is busy (ETXTBSY) and can't be
  # overwritten in place; we swap it in after stopping the service below.
  ubivm scp dist/r2proxy ":$REMOTE_DIR/r2proxy.new"

  # Write the environment file (bootstrap creds + admin token) on the VM.
  local token="${R2PROXY_ADMIN_TOKEN:-}"
  local envfile; envfile="$(mktemp)"
  cat > "$envfile" <<EOF
R2PROXY_CONFIG=$REMOTE_DIR/r2proxy.json
R2PROXY_LISTEN=0.0.0.0:8080
R2PROXY_ADMIN_LISTEN=0.0.0.0:8081
R2PROXY_ADMIN_TOKEN=$token
R2PROXY_ENDPOINT=${R2PROXY_ENDPOINT:-}
R2PROXY_ACCESS_KEY=${R2PROXY_ACCESS_KEY:-}
R2PROXY_SECRET_KEY=${R2PROXY_SECRET_KEY:-}
R2PROXY_REGION=${R2PROXY_REGION:-auto}
EOF
  ubivm scp "$envfile" ":$REMOTE_DIR/r2proxy.env"
  rm -f "$envfile"

  local unit; unit="$(mktemp)"
  cat > "$unit" <<EOF
[Unit]
Description=r2proxy
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$REMOTE_DIR/r2proxy.env
ExecStart=$REMOTE_DIR/r2proxy serve
Restart=always
RestartSec=2
User=ubi
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
  ubivm scp "$unit" ":$REMOTE_DIR/r2proxy.service"
  rm -f "$unit"

  say "installing + (re)starting systemd service"
  ubivm ssh -- "sudo systemctl stop r2proxy 2>/dev/null; \
    mv $REMOTE_DIR/r2proxy.new $REMOTE_DIR/r2proxy && chmod +x $REMOTE_DIR/r2proxy && \
    sudo mv $REMOTE_DIR/r2proxy.service /etc/systemd/system/ && sudo chmod 600 $REMOTE_DIR/r2proxy.env && \
    sudo systemctl daemon-reload && sudo systemctl enable r2proxy && sudo systemctl restart r2proxy"
  sleep 2
}

public_ip(){ ubivm show -f ip4 2>/dev/null | grep -oE '([0-9]+\.){3}[0-9]+' | head -1; }

cmd_info(){
  local ip; ip="$(public_ip)"
  say "r2proxy on $NAME ($ip)"
  echo "  proxy (S3 data-plane) : http://$ip:8080"
  echo "  admin console + API   : http://$ip:8081"
  echo
  echo "  --- first-run credentials (from service logs) ---"
  ubivm ssh -- "sudo journalctl -u r2proxy --no-pager | grep -E 'admin token|proxy access|proxy secret|tenant token|tenant id|upstream' | head -20" 2>/dev/null || true
}

case "${1:-up}" in
  up)      build; ensure_firewall; ensure_subnet; ensure_vm; deploy_binary; cmd_info ;;
  push)    build; deploy_binary; say "redeployed"; cmd_info ;;
  info)    cmd_info ;;
  logs)    ubivm ssh -- "sudo journalctl -u r2proxy -f" ;;
  ssh)     ubivm ssh ;;
  destroy)
    say "destroying VM/subnet/firewall for $NAME"
    ubi vm "$LOCATION/$NAME" destroy -f 2>/dev/null || true
    sleep 3
    ubi ps "$LOCATION/$NET" destroy -f 2>/dev/null || true
    ubi fw "$LOCATION/$FW" destroy -f 2>/dev/null || true
    ;;
  *) echo "usage: $0 {up|push|info|logs|ssh|destroy}" >&2; exit 2 ;;
esac
