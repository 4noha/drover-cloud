# drover-cloud SETUP — クラウドを一から立てる

このリポジトリの内容だけで、セッション同期クラウド（Firestore ＋ Cloud Run
relay ＋ Google ログイン Web UI）を GCP に立てられる。立てたら PC 側ツール
（[herdr-drover](../herdr-drover)）を `enroll` でつなぐ。

> **既にクラウドがある場合**（他 PC が使っている等）は、ここは飛ばして
> PC 側で「Web『＋ 端末を追加』→ `herdr-drover enroll`」だけでよい
> （[herdr-drover/SETUP.md](../herdr-drover/SETUP.md)）。サーバは 1 つを全 PC で共有する。

## 前提

- `gcloud` CLI（`gcloud auth login` 済み）＋ **課金有効な GCP プロジェクト 1 つ**
- ⚠ **対外・課金操作**。min-instances=0＋near-$0 設計だが、実 GCP リソースを作る
- （ローカルビルド確認用に）Go 1.25＋Homebrew。Cloud Build はクラウド内でビルドするので必須ではない

## 手順

### 1. 基盤 ＋ relay をデプロイ（ワンショット）

```sh
cd drover-cloud
deploy/deploy.sh <PROJECT_ID> [REGION=asia-northeast1] [SERVICE=claude-master-relay]
```

これが行うこと（`deploy/deploy.sh` の実処理）:

1. API 有効化（run / firestore / cloudbuild / artifactregistry）
2. **Firestore Native DB** 作成（未作成なら。location=REGION）
3. Artifact Registry リポジトリ `cm` 作成
4. Cloud Build で `cmd/relay/Dockerfile`（CGO 不要・distroless・静的）をビルド＆push
5. Cloud Run ランタイム SA（compute default）へ `roles/datastore.user` 付与
   ＝**サーバが Firestore を読む権限**（ADC）
6. Cloud Run デプロイ（`min-instances=0` / `cpu-throttling` / `timeout=3600` /
   `allow-unauthenticated` / `PORT=8080`）＋ env `GCP_PROJECT` と
   `WEB_SIGNING_KEY`（cookie 署名鍵。`~/.claude-master/web_signing_key` に
   固定保持＝再デプロイで変えると既存 cookie が失効するので不変に保つ）

最後に relay URL が出る。`https://…` を **`wss://…`** に読み替えたものが
`CLOUD_RELAY_URL`（PC 側 enroll で使う）。

これだけで **Firestore 一覧同期と Web ターミナルの基盤**は動く。以降 2〜4 は
Web UI にログインして使うための追加設定（`gcloud run services update` で env を足す）。

### 2. Google ログイン（Web UI の認証）

Web 管理 UI はオーナーの Google アカウントでログインする（`webauth` が ID
トークンを検証し、許可メールにのみ HMAC cookie を発行）。

1. [GCP Console → APIs & Services → OAuth 同意画面] を **External / Testing** で作成し、
   自分の Google アカウントを **テストユーザー**に追加
2. [認証情報 → OAuth クライアント ID → ウェブアプリケーション] を作成
   （承認済み JavaScript 生成元 ＝ relay の `https://…` URL）→ **クライアント ID** を控える
3. Cloud Run に env を追加:

```sh
gcloud run services update <SERVICE> --project=<PROJECT_ID> --region=<REGION> \
  --update-env-vars="GOOGLE_OAUTH_CLIENT_ID=<web-client-id>,ALLOWED_EMAILS=you@example.com"
```

`ALLOWED_EMAILS` はカンマ区切りの許可オーナー。ここに無いメールはログイン拒否。

### 3. 「＋ 端末を追加」用の enroll SA 鍵

Web の「＋ 端末を追加」は、新しい PC へ Firestore 書込用の SA 鍵を一回限りコード
経由で配布する。その配布物になる SA を用意する:

```sh
PROJECT=<PROJECT_ID>
gcloud iam service-accounts create cm-agent --project="$PROJECT" \
  --display-name="drover cloud agent"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:cm-agent@${PROJECT}.iam.gserviceaccount.com" \
  --role="roles/datastore.user" --condition=None
gcloud iam service-accounts keys create /tmp/sa.json \
  --iam-account="cm-agent@${PROJECT}.iam.gserviceaccount.com"

# base64 にして Cloud Run env へ（改行なし base64）
gcloud run services update <SERVICE> --project="$PROJECT" --region=<REGION> \
  --update-env-vars="ENROLL_SA_JSON_B64=$(base64 < /tmp/sa.json | tr -d '\n')"
rm -f /tmp/sa.json   # env へ入れたらローカルの平文鍵は消す
```

（`ENROLL_SA_JSON`＝生 JSON でも可。鍵ローテーションは `deploy/rotate-sa.sh` を参照。
⚠ ローテーションすると旧鍵で enroll 済みの他 PC は切断＝再 enroll が要る。）

### 4.（任意）Web の自動 push 復帰（M9・Firebase）

アイドルで切れた Web を、ネイティブ同様の onSnapshot push で自動復帰させる機能。
無効でも Web は動く（従来の手動再接続になるだけ）。

1. GCP プロジェクトで **Firebase を有効化** → Web アプリ登録 → 公開 config（`apiKey` 等）を取得
2. `deploy/firestore.rules` を deploy（`cm-owner` が `pcs/**` を read-only・他全拒否。
   サーバ SDK は rules 非対象＝ネイティブ同期に無影響）。firebaserules REST または
   `firebase deploy --only firestore:rules`
3. Cloud Run に env 追加:

```sh
gcloud run services update <SERVICE> --project=<PROJECT_ID> --region=<REGION> \
  --update-env-vars="FIREBASE_API_KEY=<apiKey>,FIREBASE_WEB_CONFIG_B64=$(printf '%s' '<firebase-web-config-json>' | base64 | tr -d '\n')"
```

未設定なら `/api/fbtoken` が 404＝push 無しの従来動作（安全なフォールバック）。

### 5. 確認

- `https://<relay>/` を実ブラウザで開く → Google ログイン → 端末一覧
- PC をつなぐ: [herdr-drover/SETUP.md](../herdr-drover/SETUP.md) の「クラウドに参加」

## 環境変数リファレンス（Cloud Run に設定するもの）

| env | 必須 | 用途 |
|---|---|---|
| `GCP_PROJECT` | ✅ | Firestore プロジェクト |
| `WEB_SIGNING_KEY` | ✅（Web） | HMAC cookie 署名鍵（不変に保つ） |
| `PORT` | 自動 | Cloud Run が注入（8080） |
| `GOOGLE_OAUTH_CLIENT_ID` | Web ログイン | Google Sign-In の Web クライアント ID |
| `ALLOWED_EMAILS` | Web ログイン | 許可オーナー（カンマ区切り） |
| `ENROLL_SA_JSON_B64` / `ENROLL_SA_JSON` | 端末追加 | 「＋ 端末を追加」で配布する SA 鍵 |
| `FIREBASE_API_KEY` / `FIREBASE_WEB_CONFIG_B64` | 任意（M9 push） | Web の onSnapshot 自動復帰。未設定＝push 無し |
| `GOOGLE_APPLICATION_CREDENTIALS` | 任意 | サーバ SA 明示指定（既定は Cloud Run ランタイム SA の ADC） |

## 運用

- **再デプロイ / 機能追加**: コード修正 → `deploy/deploy.sh <PROJECT>` を再実行
  （env は保持される。`WEB_SIGNING_KEY` は不変ファイルから復元）
- **SA 鍵ローテーション**: `deploy/rotate-sa.sh <PROJECT>`（⚠ 他 PC 再 enroll 要）
- **near-$0**: min-instances=0。アイドル時は課金ほぼゼロ。同期量に比例

> 注: `deploy.sh` / `rotate-sa.sh` の既定名（サービス `claude-master-relay`・
> Artifact Registry `cm`・鍵パス `~/.claude-master/web_signing_key`）は cm 由来。
> 動作に問題はないが、別名で運用したければ引数・スクリプトで変更してよい。
