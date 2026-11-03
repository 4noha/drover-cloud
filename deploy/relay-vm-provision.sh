#!/bin/sh
# relay VM へ relay バイナリ・env・systemd・Caddyfile を配置して起動する（この Mac から実行）。
# 前提: gcp-relay-vm-setup.sh 済み・Cloudflare に relay.4noha.org の A レコード（DNS only）。
set -eu
P=claude-master-4noha; Z=asia-northeast1-b; HOST=relay.4noha.org
BIN=${BIN:-$(cd "$(dirname "$0")/.." && pwd)/dist/relay_linux_amd64}
[ -f "$BIN" ] || { echo "relay バイナリが無い: $BIN"; exit 1; }
# slave 認可用の SA JSON（b64）は Cloud Run の env から取り出して VM の env ファイルへ（画面には出さない）
SA_B64=$(gcloud run services describe claude-master-relay --project=$P --region=asia-northeast1 --format=json | python3 -c 'import json,sys; env=json.load(sys.stdin)["spec"]["template"]["spec"]["containers"][0]["env"]; print(next(e["value"] for e in env if e["name"]=="ENROLL_SA_JSON_B64"))')
[ -n "$SA_B64" ] || { echo "ENROLL_SA_JSON_B64 を取れない"; exit 1; }
echo "== binary scp"; gcloud compute scp "$BIN" relay:/tmp/drover-relay --project=$P --zone=$Z --quiet
echo "== env/systemd/caddy"
printf 'GCP_PROJECT=%s\nRELAY_ONLY=1\nPORT=8080\nENROLL_SA_JSON_B64=%s\n' "$P" "$SA_B64" | gcloud compute ssh relay --project=$P --zone=$Z --quiet --command='sudo install -d -m 700 -o root -g root /etc/drover && sudo tee /etc/drover/relay.env >/dev/null && sudo chmod 600 /etc/drover/relay.env'
gcloud compute ssh relay --project=$P --zone=$Z --quiet --command="sudo install -d /opt/drover && sudo install -m 755 /tmp/drover-relay /opt/drover/relay && rm -f /tmp/drover-relay
sudo tee /etc/systemd/system/drover-relay.service >/dev/null <<'UNIT'
[Unit]
Description=drover relay (WS relay only)
After=network-online.target
Wants=network-online.target
[Service]
EnvironmentFile=/etc/drover/relay.env
ExecStart=/opt/drover/relay
Restart=always
RestartSec=2
User=drover
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
[Install]
WantedBy=multi-user.target
UNIT
sudo tee /etc/caddy/Caddyfile >/dev/null <<CADDY
$HOST {
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080
}
CADDY
sudo systemctl daemon-reload && sudo systemctl enable --now drover-relay && sudo systemctl restart caddy && sleep 2 && systemctl is-active drover-relay caddy && curl -s http://127.0.0.1:8080/"
echo "== TLS（Let's Encrypt は DNS が通ってから数十秒）"; sleep 5; curl -s --max-time 20 https://$HOST/ || echo "（まだ。DNS/証明書を待って再実行: curl https://$HOST/）"
