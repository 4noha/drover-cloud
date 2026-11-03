//go:build windows

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// placeBinary（Windows）: 走行中の exe は削除できないが**改名はできる**ので、
// exe → exe.old へ退避してから新バイナリを exe に置く。
//
// ⚠ 実障害（2026-11-03・desktop-djb9pfr）: `rename exe → exe.old: Access is denied`。
// 前回の更新で退避した exe.old がまだ走行中（旧 daemon が exit し切っていない／
// 周期タスクが旧イメージを起動した）だと、os.Remove(old) が黙って失敗し、同名への
// 改名も拒否される。退避名が塞がっていれば時刻付きの別名（exe.old-<unix>）へ退避する。
// 残骸（exe.old*）は次回以降の更新で best-effort に掃除する（ロック中なら残る）。
func placeBinary(tmpName, exe string) error {
	cleanupOldBackups(exe)
	old := exe + ".old"
	if err := os.Rename(exe, old); err != nil && !os.IsNotExist(err) {
		alt := fmt.Sprintf("%s.old-%d", exe, time.Now().Unix())
		if err2 := os.Rename(exe, alt); err2 != nil {
			return fmt.Errorf("退避失敗 %s: %w（別名 %s も失敗: %v）", exe, err, filepath.Base(alt), err2)
		}
		old = alt
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Rename(old, exe) // ロールバック
		return fmt.Errorf("置換失敗 %s: %w", exe, err)
	}
	_ = os.Remove(old) // best-effort（走行中イメージがロック中なら残す）
	return nil
}

// cleanupOldBackups は exe.old / exe.old-* を best-effort で消す（ロック中は残る＝無視）。
func cleanupOldBackups(exe string) {
	dir, base := filepath.Split(exe)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, base+".old") {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}
