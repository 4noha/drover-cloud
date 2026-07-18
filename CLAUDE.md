# drover-cloud プロジェクト

セッション同期クラウドの **共有バックエンド**。端末マルチプレクサ（herdr /
tmux 系）のセッションを複数 PC・ブラウザ・スマホへ同期する Firestore 状態 ＋
Cloud Run relay（WSS）＋ Google ログイン Web UI と、その **共有 Go クライアント
ライブラリ**（`state`/`relayclient`/`selfupdate`）。

- 何が入っているか・構成: [README.md](README.md)
- クラウドを一から立てる手順: **[SETUP.md](SETUP.md)**

## 由来と位置づけ（重要）

- `claude-master-go`（cm）で実証・稼働中のクラウド層を**独立デプロイ・再利用
  できる形に切り出した**リポジトリ。サーバ本番コード（relay/web/webauth/state）は
  **cm とバイト等価のコピー**、共有クライアント lib は cm と herdr-drover に
  二重に存在していたバイト同一コピーを**一本化**したもの。
- **cm は凍結（新規投資なし）**。稼働中の Cloud Run relay は cm ビルド由来だが、
  本 repo はその動くコードのコピー＝以後の再デプロイ・機能追加は**本 repo が正典**。
- consumer: [herdr-drover](../herdr-drover)（`state`/`relayclient`/`selfupdate` を
  import）。開発中は consumer 側 go.mod の `replace => ../drover-cloud` で解決。

## パッケージ構成

- **公開・共有 lib**（consumer が import）: `state`（Firestore）／`relayclient`
  （agent dial-out）／`selfupdate`
- **サーバ本体**: `relay`（byte 透過 WSS）／`web`（Google ログイン Web UI・
  端末一覧・Web ターミナル・enroll・遠隔命令・Firebase push token）／`webauth`
  （HMAC cookie＋Google ID トークン検証）
- **デプロイ物**: `cmd/relay`（Cloud Run に載る単一バイナリ）＋ `cmd/relay/Dockerfile`
- **deploy/**: `deploy.sh`（ワンショット）／`cloudbuild.yaml`／`firestore.rules`／
  `rotate-sa.sh`

## 開発の鉄則

1. **推測修正をしない** — 実再現してから直す
2. **実テストで担保** — 実 Firestore エミュレータ・実 relay。合成で緑にしない。
   修正前に旧コードでテストが落ちることを確認
3. **サーバ本番コードは cm とバイト等価を保つ**。cm 側の display-oracle e2e
   （`relay_test.go`/`web_test.go`）は **VT モデル（cm の `internal/screen`）依存**
   のため本 repo には移植しない＝**cm 側のゲートがサーバ描画を担保する**
   （本 repo の relay/web は VT 非依存の単体テストのみ。fbtoken 等の認可系は
   `web/webhelpers_test.go` に VT 非依存ヘルパを抽出して緑）。

## 絶対の禁則

- **cm リポジトリ（`~/works/tools/claude-master-go`）を改変しない**（読むだけ）。
  稼働中の Cloud Run relay も無停止で保つ（再デプロイはユーザー明示確認後）。
- SA 鍵（enroll 配布物・`ENROLL_SA_JSON_B64`）はリポジトリ外・非コミット。
- 実クラウドへのデプロイ・SA 鍵ローテーション（`rotate-sa.sh`）は**対外・課金・
  一部不可逆**＝ユーザー明示確認後に実行。

## サーバが読む env（Cloud Run に設定）

`GCP_PROJECT`（必須）／`WEB_SIGNING_KEY`（Web cookie 署名・不変に保つ）／
`PORT`（Cloud Run 注入）／`GOOGLE_OAUTH_CLIENT_ID`・`ALLOWED_EMAILS`（Google
ログイン）／`ENROLL_SA_JSON_B64`|`ENROLL_SA_JSON`（端末追加の SA 配布）／
`FIREBASE_API_KEY`・`FIREBASE_WEB_CONFIG_B64`（任意・M9 push）／
`GOOGLE_APPLICATION_CREDENTIALS`（任意・既定は Cloud Run ランタイム SA の ADC）。
詳細は [SETUP.md](SETUP.md)。

## ビルド / テスト / デプロイ

```sh
export PATH="/opt/homebrew/bin:$PATH"          # Homebrew Go
gofmt -l . | grep -v static/ ; go build ./... && go vet ./... && go test ./... -count=1
# 3-OS build（relay は Cloud Run=linux で動く。selfupdate の place は unix のみ）
GOOS=linux go build ./...
# 実デプロイ（対外・課金＝要確認）
deploy/deploy.sh <PROJECT_ID> [REGION] [SERVICE]
```

⚠ near-$0 設計（min-instances=0）。`state` の Firestore エミュレータテストは
`gcloud` エミュレータ（実 API）を使う＝合成しない。
