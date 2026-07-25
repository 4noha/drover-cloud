package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 静的ファイルは **必ず検証子（ETag）付きで配信**しなければならない。
//
// embed.FS は modtime が 0 なので http.FileServer は Last-Modified を出さず、
// 何も設定しなければ ETag も付かない＝**検証子ゼロ**。ブラウザがヒューリスティックに
// キャッシュするため、**UI を直してデプロイしても利用者に届かない**
// （実障害 2026-07-25: 再起動ボタンの文言変更が反映されなかった）。
func TestStaticServedWithValidator(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	h := http.StripPrefix("/static/", staticHandler(sub))

	for _, name := range []string{"devices.js", "term.js"} {
		req := httptest.NewRequest(http.MethodGet, "/static/"+name, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", name, rec.Code)
		}
		et := rec.Header().Get("ETag")
		if et == "" {
			t.Fatalf("%s: ETag が無い＝ブラウザが古い JS を握り続ける", name)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control=%q（no-cache＝毎回再検証にする）", name, cc)
		}

		// 同じ ETag なら 304（本文を流さない＝転送量は増えない）。
		req2 := httptest.NewRequest(http.MethodGet, "/static/"+name, nil)
		req2.Header.Set("If-None-Match", et)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotModified {
			t.Fatalf("%s: 再検証で %d（304 のはず）", name, rec2.Code)
		}

		// ETag が違えば 200 で本文が流れる（＝更新が届く）。
		req3 := httptest.NewRequest(http.MethodGet, "/static/"+name, nil)
		req3.Header.Set("If-None-Match", `"stale00000000000"`)
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusOK || rec3.Body.Len() == 0 {
			t.Fatalf("%s: 古い ETag で %d body=%d（200＋本文のはず）",
				name, rec3.Code, rec3.Body.Len())
		}
	}
}

// ETag は内容から決まる＝内容が違えば必ず違う。
func TestStaticETagsAreContentBased(t *testing.T) {
	sub, _ := fs.Sub(staticFS, "static")
	staticHandler(sub) // ハッシュを計算させる
	if len(staticETags) < 2 {
		t.Fatalf("ETag が計算されていない: %v", staticETags)
	}
	seen := map[string]string{}
	for name, et := range staticETags {
		if prev, dup := seen[et]; dup {
			b1, _ := fs.ReadFile(sub, name)
			b2, _ := fs.ReadFile(sub, prev)
			if string(b1) != string(b2) {
				t.Errorf("内容が違うのに ETag が同じ: %s / %s", name, prev)
			}
		}
		seen[et] = name
	}
}
