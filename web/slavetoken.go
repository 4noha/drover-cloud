package web

// slave（共用 PC）用の bearer トークン: uid="slave:<pc>"・RS256・1h。
// owner 用の firebaseCustomToken を uid だけ差し替えて再利用する（トークン
// 形状は 1 つ＝genuine Firebase custom token のまま。将来 firestore.rules の
// slave:<pc> パスを Identity Toolkit 経由で到達可能にする防御多重の土台）。
// 主データ面の検証は relay 自身が RS256 署名 + uid + exp で行う（aud は
// **検証しない**＝relay は Identity Toolkit ではない）。fbtoken.go は無改変。

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// mintSlaveToken は SA 秘密鍵で署名した slave bearer（uid="slave:<pc>"）を作る。
// firebaseCustomToken（fbtoken.go:36）の uid を差し替えるだけ＝owner path 無改変。
func mintSlaveToken(saJSON []byte, pc string, now time.Time) (string, error) {
	return firebaseCustomToken(saJSON, "slave:"+pc, now)
}

// saPublicKey は SA 鍵 JSON の private_key（PKCS8 PEM）から RSA 公開鍵を
// 導出する（firebaseCustomToken と同一 parse ロジックを複製＝fbtoken.go を
// 触らない・§8.8 invariant 死守）。relay は s.enrollSA で完全な SA JSON を
// 既に保持しているので、ネットワーク往復無しで検証できる。
func saPublicKey(saJSON []byte) (*rsa.PublicKey, error) {
	var sa struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(saJSON, &sa); err != nil {
		return nil, fmt.Errorf("SA 鍵 JSON: %w", err)
	}
	if sa.PrivateKey == "" {
		return nil, fmt.Errorf("SA 鍵 JSON に private_key が無い")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("SA private_key が PEM でない")
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SA private_key parse: %w", err)
	}
	rsaKey, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("SA private_key が RSA でない")
	}
	return &rsaKey.PublicKey, nil
}

// verifySlaveToken は slave bearer を SA 公開鍵で RS256 検証し、束縛 pc
// （"mac-studio-herdr" 等）を返す。sig → exp → uid("slave:" 接頭辞) の順で
// 検証し、aud は見ない（relay は Identity Toolkit ではない）。
func verifySlaveToken(saJSON []byte, tok string, now time.Time) (pc string, err error) {
	pub, err := saPublicKey(saJSON)
	if err != nil {
		return "", err
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("JWT が 3 セグメントでない")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("署名 decode: %w", err)
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
		return "", fmt.Errorf("RS256 署名検証失敗: %w", err)
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("claims decode: %w", err)
	}
	var claims struct {
		Exp int64  `json:"exp"`
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(cb, &claims); err != nil {
		return "", fmt.Errorf("claims JSON: %w", err)
	}
	if claims.Exp == 0 || now.Unix() >= claims.Exp {
		return "", fmt.Errorf("expired")
	}
	const pfx = "slave:"
	if !strings.HasPrefix(claims.UID, pfx) {
		return "", fmt.Errorf("uid が slave でない")
	}
	pc = claims.UID[len(pfx):]
	if pc == "" {
		return "", fmt.Errorf("uid の pc が空")
	}
	return pc, nil
}

// bearer は Authorization: Bearer <tok> の tok を取り出す（無ければ ""）。
// この関数の戻り値が非空か否かが「slave 経路か master/owner 経路か」の判定。
func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(a) > len(pfx) && strings.EqualFold(a[:len(pfx)], pfx) {
		return strings.TrimSpace(a[len(pfx):])
	}
	return ""
}

// slaveSessionKey は slave の pairing key を pc で名前空間化する（NUL は
// sid/URL に現れない）。二つの PC が同じ sid 文字列を push しても slot が
// 衝突しない＝slave が owner の source slot を hijack できない（§2.10）。
func slaveSessionKey(pc, sid string) string { return pc + "\x00" + sid }

// sha256Hex は s の sha256 を hex で返す（slave secret のハッシュ照合用）。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
