# drover-cloud

**セッションのクラウド同期バックエンド** — 端末マルチプレクサ（herdr /
tmux 系）のセッション群を、複数 PC・ブラウザ・スマホへ Firestore push ＋
Cloud Run relay（WSS）で「駆り立てる」ための共有クラウド層。

`claude-master-go`（cm）で実証したクラウド同期（Firestore 状態同期・
byte 透過 WSS relay・Google ログインの Web UI・near-$0）を、**独立して
デプロイ・再利用できる形**に切り出した standalone リポジトリ。
[herdr-drover](https://github.com/4noha/herdr-drover)（herdr プラグイン）の依存先であり、
今後のツールもここを共有する。

## 構成

| 種別 | パッケージ | 役割 |
|---|---|---|
| **サーバ（デプロイ物）** | `cmd/relay` | Cloud Run に載る単一バイナリ（relay＋Web を 1 プロセスで提供） |
| | `relay` | sid 単位の byte 透過 WSS relay（viewer ⇔ PC agent をトンネル） |
| | `web` | Google ログイン Web 管理 UI（端末一覧・Web ターミナル・enroll・遠隔命令・Firebase push token） |
| | `webauth` | HMAC cookie 署名＋Google ID トークン検証 |
| **共有クライアント lib** | `state` | Firestore 状態（PushStatus/DeleteSession/WatchWake/WatchSessions/PutRelayGrant/commands） |
| | `relayclient` | agent 側 dial-out（relay へ WSS 接続・quiescence 自切断） |
| | `selfupdate` | GitHub Releases 自己更新（sha256 検証・原子置換） |
| **デプロイ** | `deploy/` | `deploy.sh`（ワンショット）／`cloudbuild.yaml`／`firestore.rules`／`rotate-sa.sh` |

- `state`・`relayclient`・`selfupdate` は public パッケージ＝**consumer
  （herdr-drover 等）が import する共有ライブラリ**。以前は cm と
  herdr-drover に byte 同一コピーが二重に存在していたのを本 repo に一本化。
- `relay`・`web`・`webauth` はサーバ本体。`cmd/relay` だけがそれらを束ねる。

## クラウドを立てる / つなぐ

- **一から立てる**（GCP プロジェクト・Firestore・Cloud Run relay・Google
  ログイン・enroll・push）: **[SETUP.md](SETUP.md)** の手順どおり。
- **既存クラウドにつなぐ**: PC 側ツール（herdr-drover）で
  Web「＋ 端末を追加」→ `enroll` するだけ。サーバ側の操作は不要。

## consumer との関係

```
drover-cloud（この repo）
 ├─ サーバ: cmd/relay を Cloud Run へ 1 回デプロイ（全 PC・全ツールで共有）
 └─ 共有 lib: state / relayclient / selfupdate
      └─ import ← herdr-drover（herdr プラグイン。PC 側 agent＋claude シム）
```

cm（claude-master-go）は凍結（新規投資なし）。稼働中の Cloud Run relay は
cm のビルド由来だが、本 repo は**その動くコードのコピー＝バイト等価**であり、
以後の再デプロイ・機能追加は本 repo を正典とする（cm は無改変・無停止）。

## ビルド / テスト

```sh
export PATH="/opt/homebrew/bin:$PATH"   # Homebrew Go
go build ./... && go vet ./...
go test ./...                            # state は Firestore エミュレータ、
                                         # relay/web の display-oracle e2e は
                                         # VT モデル依存のため cm に残置（本番
                                         # コードはバイト等価＝cm のゲートが担保）
```

## 開発の鉄則（cm/herdr-drover から継承）

1. 推測修正をしない — 実再現してから直す
2. 実テストで担保 — 実 Firestore エミュレータ・実 relay。合成で緑にしない
3. サーバ本番コードは cm とバイト等価を保つ（display-oracle は cm 側で担保）
