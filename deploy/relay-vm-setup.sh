#!/bin/sh
# drover relay VM（東京 e2-micro）の作成。冪等（既にあるものは skip）。
set -u
P=claude-master-4noha; R=asia-northeast1; Z=asia-northeast1-b
echo "== [1/6] Compute Engine API"; gcloud services enable compute.googleapis.com --project=$P
echo "== [2/6] サービスアカウント relay-vm（Firestore 読み書き＝grant/wake の検証用。鍵ファイル不要）"
gcloud iam service-accounts describe relay-vm@$P.iam.gserviceaccount.com --project=$P >/dev/null 2>&1 || gcloud iam service-accounts create relay-vm --project=$P --display-name="drover relay VM"
gcloud projects add-iam-policy-binding $P --member="serviceAccount:relay-vm@$P.iam.gserviceaccount.com" --role=roles/datastore.user --condition=None >/dev/null && echo "  datastore.user 付与"
echo "== [3/6] 固定 IP relay-ip"; gcloud compute addresses describe relay-ip --project=$P --region=$R >/dev/null 2>&1 || gcloud compute addresses create relay-ip --project=$P --region=$R
IP=$(gcloud compute addresses describe relay-ip --project=$P --region=$R --format='value(address)'); echo "  IP: $IP"
echo "== [4/6] ファイアウォール（tcp 80/443 → タグ relay）"; gcloud compute firewall-rules describe relay-allow-web --project=$P >/dev/null 2>&1 || gcloud compute firewall-rules create relay-allow-web --project=$P --network=default --direction=INGRESS --action=ALLOW --rules=tcp:80,tcp:443 --target-tags=relay --source-ranges=0.0.0.0/0
echo "== [5/6] 起動スクリプト（caddy 導入・drover ユーザ作成）"
cat > /tmp/relay-startup.sh <<'EOS'
#!/bin/bash
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q caddy
mkdir -p /opt/drover /etc/drover
id -u drover >/dev/null 2>&1 || useradd --system --home /opt/drover --shell /usr/sbin/nologin drover
EOS
echo "== [6/6] VM relay（e2-micro・Debian 12・pd-balanced 10GB）"
gcloud compute instances describe relay --project=$P --zone=$Z >/dev/null 2>&1 || gcloud compute instances create relay --project=$P --zone=$Z --machine-type=e2-micro --image-family=debian-12 --image-project=debian-cloud --boot-disk-size=10GB --boot-disk-type=pd-balanced --address=relay-ip --tags=relay --service-account="relay-vm@$P.iam.gserviceaccount.com" --scopes=cloud-platform --metadata-from-file=startup-script=/tmp/relay-startup.sh
gcloud compute instances describe relay --project=$P --zone=$Z --format='value(name,status,networkInterfaces[0].accessConfigs[0].natIP)'
echo "== 完了。Cloudflare に A レコード（DNS only・プロキシ OFF）: relay.<ドメイン> → $IP"
