package state

// resident watcher の耐障害ループ。Firestore Snapshots の it.Next() は
// ctx 以外の理由でストリームが終端すると iterator.Done
// （"no more items in iterator"）を返す。旧実装はこれを致命 return し、
// WatchWake→ag.Run→runCloudAgent.exitErr で **agent プロセスが終了**。
// Mac は launchd KeepAlive で即復帰し症状が隠蔽されていたが、Windows
// S4U（AtLogOn のみ・KeepAlive 無し）では復帰せず＝クラウド同期が死ぬ
// 真因。resident な制御線は終端で死なず **再購読** すべき。
//
// keepSubscribed は ctx 終了でのみ nil を返し、それ以外（iterator.Done
// 含む任意のエラー）は購読を Stop→指数バックオフ→再購読する。protocol/
// 不変条件は不変（制御線の耐障害性のみ）。純粋（ctx＋クロージャ＋time）
// ＝fake で決定論ユニットテスト可能（実 Firestore で Done を強制するのは
// 非決定的なため、再購読ロジック自体を実コードのまま検証する）。

import (
	"context"
	"errors"
	"log"
	"time"
)

const (
	watchBackoffMin = 200 * time.Millisecond
	watchBackoffMax = 5 * time.Second

	// watchMaxLifetime: 1 購読の寿命上限。Firestore Snapshots() の it.Next() は
	// ストリームが**エラーを返さず無言で死ぬ**（ネットワーク断後に TCP RST が来ず
	// gRPC 側も検知できない既知動作。keepalive 未設定のため）と永遠にブロックし、
	// pump 自体が致命的にハングする＝このファイル上部が想定する「終端 →
	// 再購読」の前提（pump がいつか return する）が崩れる。2026-10-17 22:40 JST
	// の実インシデントで実測: commands watcher がこれで90分超ハングし、
	// Grok Bot 発の read-pane/send-input が一切処理されず溜まり続けた。
	// 寿命上限を設け、ctx 期限切れで強制終端させることで、無言ハングの実害を
	// 最大 watchMaxLifetime 分に制限する（Firestore の内部 RecvMsg は ctx-aware
	// ＝キャンセルで Next() が確実に戻ることを実測確認済み。watch_test.go 参照）。
	watchMaxLifetime = 5 * time.Minute
)

// sleepCtx は d 待つ。ctx 終了で false（待たず中断）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// keepSubscribed は subscribe で購読を作り pump() を回し切り、終端したら
// stop()→backoff→再購読、を ctx 終了まで繰り返す。pump は 1 購読分を
// 処理（各スナップショットで cb 等を呼ぶ）し、終端理由を返す。戻り値は
// ctx 終了時のみ nil（resident は「終端で死なない」＝致命 return しない）。
//
// subscribe には ctx 自体ではなく、maxLifetime で期限を切った派生 ctx
// （subCtx）を渡す（maxLifetime<=0 なら無期限＝従来どおり ctx をそのまま
// 渡す）。pump が無言ハングしても subCtx の期限切れで Next() 等が強制的に
// エラー終端し、このループへ戻ってくる（watchMaxLifetime のコメント参照）。
// 強制終端時は silent にしない（鉄則5）: 2026-10-17 のインシデントが
// agent.log に一切残らなかった反省で log.Printf する。
func keepSubscribed(ctx context.Context, maxLifetime time.Duration,
	subscribe func(subCtx context.Context) (pump func() error, stop func())) error {
	bo := watchBackoffMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		subCtx := ctx
		cancel := func() {}
		if maxLifetime > 0 {
			subCtx, cancel = context.WithTimeout(ctx, maxLifetime)
		}
		pump, stop := subscribe(subCtx)
		err := pump()
		stop()
		timedOut := errors.Is(subCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if timedOut {
			log.Printf("[state] watch stream %s 無応答＝強制再購読（pump戻り値: %v）", maxLifetime, err)
		}
		if !sleepCtx(ctx, bo) {
			return nil
		}
		if bo *= 2; bo > watchBackoffMax {
			bo = watchBackoffMax
		}
	}
}
