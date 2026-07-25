//go:build windows

package selfupdate

import (
	"fmt"
	"os"
)

// placeBinary は新バイナリ(tmp)を実行中バイナリ位置へ差し替える（Windows 版）。
// Windows は実行中 exe を rename で **上書き**できない（イメージがロック）が、
// 実行中 exe を別名へ **退避 rename** することは許される。よって
// exe→exe.old へ退避してから tmp→exe へ移し、旧版は best-effort で消す
// （走行中イメージが掴んでいれば残るが次回起動で消える）。unix の原子 rename
// （place_unix.go）に対する Windows 相当。
func placeBinary(tmpName, exe string) error {
	old := exe + ".old"
	_ = os.Remove(old) // 前回の残骸があれば除去（無くても可）
	if err := os.Rename(exe, old); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("退避失敗 %s: %w", exe, err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Rename(old, exe) // ロールバック
		return fmt.Errorf("置換失敗 %s: %w", exe, err)
	}
	_ = os.Remove(old) // best-effort（走行中イメージがロック中なら残す）
	return nil
}
