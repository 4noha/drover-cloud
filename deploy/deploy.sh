#!/usr/bin/env bash
# claude-master クラウド層デプロイ（M6）。実際に M6e で通った手順。
#
# ⚠ 対外・課金。実行前に GCP プロジェクト/課金/リージョンを確定し
#   `gcloud auth login` 済みであること。引数が無ければ何もしない。
#
# 使い方:
#   deploy/deploy.sh <PROJECT_ID> [REGION=asia-northeast1] [SERVICE=claude-master-relay]
#
# 行うこと:
#   1. API 有効化（run/firestore/cloudbuild/artifactregistry）
#   2. Firestore Native DB 作成（未作成なら）
#   3. Artifact Registry リポジトリ作成（cm）
#   4. Cloud Build で cmd/relay/Dockerfile をビルド＆push
#   5. Cloud Run デプロイ（min-instances=0 / cpu-throttling /
#      timeout=3600 / allow-unauthenticated。sid は claude UUID で
#      推測困難、wake 制御は Firestore 認証で別途防御）
#   6. relay URL を表示（CLOUD_RELAY_URL に wss:// で設定）
set -euo pipefail

PROJECT="${1:-}"
REGION="${2:-asia-northeast1}"
SERVICE="${3:-claude-master-relay}"
if [[ -z "$PROJECT" ]]; then
  sed -n '2,17p' "$0"; exit 2
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMG="${REGION}-docker.pkg.dev/${PROJECT}/cm/${SERVICE}"

echo "==> [1/5] API 有効化"
gcloud services enable run.googleapis.com firestore.googleapis.com \
  cloudbuild.googleapis.com artifactregistry.googleapis.com --project="$PROJECT"

echo "==> [2/5] Firestore Native DB（既存ならスキップ）"
gcloud firestore databases create --location="$REGION" \
  --type=firestore-native --project="$PROJECT" 2>/dev/null || \
  echo "    (既に存在)"

echo "==> [3/5] Artifact Registry リポジトリ cm"
gcloud artifacts repositories create cm --repository-format=docker \
  --location="$REGION" --project="$PROJECT" 2>/dev/null || echo "    (既に存在)"

echo "==> [4/5] Cloud Build（cmd/relay/Dockerfile）"
( cd "$ROOT" && gcloud builds submit --project="$PROJECT" \
    --config deploy/cloudbuild.yaml --substitutions=_IMAGE="$IMG" . )

echo "==> [5/6] Web 用: Cloud Run ランタイム SA に Firestore 権限"
RSA="$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')-compute@developer.gserviceaccount.com"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:${RSA}" --role="roles/datastore.user" \
  --condition=None >/dev/null 2>&1 || true

echo "==> [6/6] Cloud Run デプロイ（Web 管理 UI 有効）"
# WEB_SIGNING_KEY は ~/.claude-master/web_signing_key に固定保持
# （cookie 署名鍵。再デプロイで変えると既存 cookie が失効）。
KEYF="$HOME/.claude-master/web_signing_key"
if [[ ! -s "$KEYF" ]]; then
  mkdir -p "$HOME/.claude-master"
  openssl rand -hex 32 > "$KEYF"; chmod 600 "$KEYF"
fi
WKEY="$(cat "$KEYF")"
# ⚠ --update-env-vars（マージ）を使うこと。--set-env-vars（丸ごと置換）だと
#   既存サービスへ再実行した時に GOOGLE_OAUTH_CLIENT_ID/ALLOWED_EMAILS/
#   ENROLL_SA_JSON_B64/FIREBASE_*（SETUP.md 手順 2-4 で後から足した分）が
#   消え、Web UI が丸ごと無効化される（2026-09-28 実際に事故った。本番で
#   Web UI が落ち、直前リビジョンの env を掘って復元する羽目になった）。
#   この 2 キーだけならどちらでも新規サービスは動くが、既存サービスへの
#   再実行を常に安全にするため --update-env-vars で統一する。
gcloud run deploy "$SERVICE" --project="$PROJECT" --region="$REGION" \
  --image="$IMG" --min-instances=0 --max-instances=4 \
  --cpu-throttling --timeout=3600 --allow-unauthenticated --port=8080 \
  --update-env-vars="GCP_PROJECT=${PROJECT},WEB_SIGNING_KEY=${WKEY}"

URL="$(gcloud run services describe "$SERVICE" --project="$PROJECT" \
  --region="$REGION" --format='value(status.url)')"
echo "==> 基盤デプロイ完了。relay URL = $URL"
echo "    export CLOUD_RELAY_URL=\"${URL/https:/wss:}\"   # PC 側 enroll で使う wss://"
echo "    署名鍵=${KEYF}（再デプロイでも不変に保つ＝既存 cookie 失効防止）"
echo "    次: Google ログイン/端末追加/push の env を設定（SETUP.md 手順 2-4）:"
echo "      gcloud run services update ${SERVICE} --project=${PROJECT} --region=${REGION} \\"
echo "        --update-env-vars=GOOGLE_OAUTH_CLIENT_ID=...,ALLOWED_EMAILS=...,ENROLL_SA_JSON_B64=..."
echo "    Web UI: ${URL}/ （オーナーの Google アカウントでログイン）"
echo "    Firestore ルール: deploy/firestore.rules（サーバ SDK は rules 非対象。"
echo "    ブラウザ push=M9 を使うとき firebase deploy）。PC 接続は herdr-drover enroll。"
echo "    任意: MCP（drover-cloud-mcp-design.md）を有効化するとき（Phase 1。"
echo "    MCP_ALLOWED_REDIRECT_HOSTS は Grok Bot 等のコネクタ登録で実ホストが"
echo "    判明してから埋める＝未決事項7。空のままなら DCR は全拒否で安全側）:"
echo "      gcloud run services update ${SERVICE} --project=${PROJECT} --region=${REGION} \\"
echo "        --update-env-vars=MCP_ENABLED=1,MCP_SELF_BASE_URL=${URL},MCP_ALLOWED_REDIRECT_HOSTS=,MCP_SLAVE_ACCESS=off"
echo "    MCP_SELF_BASE_URL は上の relay URL（末尾 / 無し）。oauth メタデータの"
echo "    issuer/resource の組み立てに使う。"
echo "    Firestore TTL ポリシー（mcp_results 10分・mcp_audit 90日・oauth_codes"
echo "    60秒）は未作成。実際に有効化する時に次を実行すること（このスクリプトは"
echo "    実行しない＝本番 Firestore 設定は明示操作に限定）:"
echo "      gcloud firestore fields ttls update expire_at --collection-group=mcp_results --enable-ttl --project=${PROJECT}"
echo "      gcloud firestore fields ttls update expire_at --collection-group=mcp_audit --enable-ttl --project=${PROJECT}"
echo "      gcloud firestore fields ttls update exp --collection-group=oauth_codes --enable-ttl --project=${PROJECT}"
