//go:build windows

package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 退避名 exe.old が塞がっている（消せない・改名できない）時は別名へ退避して置換を続ける。
// Windows でしか走らない（実機: desktop-djb9pfr で `go test ./selfupdate`）。
func TestPlaceBinaryFallsBackWhenOldIsBusy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "herdr-drover.exe")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	// exe.old を「消せない・上書きできない」状態にする＝中身のあるディレクトリ。
	if err := os.MkdirAll(filepath.Join(exe+".old", "lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "new.tmp")
	if err := os.WriteFile(tmp, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := placeBinary(tmp, exe); err != nil {
		t.Fatalf("別名退避で置換できるはず: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Fatalf("置換されていない: %q", b)
	}
	ents, _ := os.ReadDir(dir)
	alt := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "herdr-drover.exe.old-") {
			alt++
		}
	}
	if alt != 1 {
		t.Fatalf("別名退避（exe.old-<unix>）が 1 つあるはず: %d", alt)
	}
}
