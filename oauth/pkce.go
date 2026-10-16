package oauth

// PKCE S256（RFC 7636）検証。authorize で受けた code_challenge を、
// token エンドポイントで受けた code_verifier と突き合わせる。

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// sha256Hex はオペークな認可コード/トークンのハッシュ化に使う（webauth.HashCode
// は人手入力の pairing code 用に大小文字・空白・ハイフンを正規化するため、
// 高エントロピーな乱数コードには意味的に別物＝専用の単純 sha256 hex を使う）。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// pkceChallengeFromVerifier は code_verifier から S256 の code_challenge を
// 導出する（base64url, パディング無し＝RFC 7636 §4.2 と同じ）。
func pkceChallengeFromVerifier(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// pkceVerify は authorize 時に保存した code_challenge と token 時に受けた
// code_verifier が一致するかを定数時間で比較する。
func pkceVerify(codeChallenge, verifier string) bool {
	if codeChallenge == "" || verifier == "" {
		return false
	}
	got := pkceChallengeFromVerifier(verifier)
	return subtle.ConstantTimeCompare([]byte(got), []byte(codeChallenge)) == 1
}
