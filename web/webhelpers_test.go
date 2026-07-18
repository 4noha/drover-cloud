package web

// webhelpers_test.go — cm（claude-master-go）の web_test.go から VT 非依存の
// テストヘルパだけを抽出したもの。cm 本体の web_test.go は display-oracle
// e2e（internal/screen〔VT〕・ptyproxy 依存）を含み drover-cloud には移植
// しない（VT モデルは cm 固有資産・本番 web.go はバイト同一＝cm のゲートが
// 担保）。ここには fbtoken_test.go／fbtoken_manual_test.go が参照する
// 認可系ヘルパ（VT 非依存）のみを置く。

import (
	"context"
	"time"

	"github.com/4noha/drover-cloud/webauth"
)

// allowEmail は許可オーナーのメール（Google 認証の allow-list）。
const allowEmail = "ok@example.com"

// fakeGV は Google ID トークン検証の差し替え（認可ロジックを決定的に）。
type fakeGV struct {
	email    string
	verified bool
	err      error
}

func (f fakeGV) Verify(_ context.Context, _, _ string) (string, bool, error) {
	return f.email, f.verified, f.err
}

// authCookie は Google 認証済（許可メール・scope=全 PC）の署名 cookie。
func authCookie(ws *Server) string {
	tok := ws.signer.Sign(webauth.Token{
		PC: allowEmail, Scope: accountScope,
		Exp: time.Now().Add(time.Hour).Unix(),
	})
	return cookieName + "=" + tok
}
