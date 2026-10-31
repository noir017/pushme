package approval

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
)

// Payload 是手机批准时签名的内容，发起方与手机各自按同一规则拼出来：
//
//	pushme-approval-v1
//	<id>
//	<challenge>
//	approve
//	<expires>
//	<sha256hex(title)>
//	<sha256hex(text)>
//
// 行间 "\n"，末尾不带换行。title、text 分别取哈希，内容在两者之间挪动也验不过。
// 发起方用的是自己发出去的 title/text，所以 pushme 改了显示内容，签名就对不上。
func Payload(a Approval) []byte {
	return []byte(strings.Join([]string{
		"pushme-approval-v1",
		a.ID,
		a.Challenge,
		"approve",
		strconv.FormatInt(a.Expires, 10),
		sha256Hex(a.Title),
		sha256Hex(a.Text),
	}, "\n"))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var ErrBadSignature = errors.New("signature does not verify")

// Verify 用手机公钥核对 a.Sig（base64 的 ASN.1 DER ECDSA，即 Android 的 SHA256withECDSA）。
func Verify(pub *ecdsa.PublicKey, a Approval) error {
	return VerifySig(pub, Payload(a), a.Sig)
}

// VerifySig 核对 sig（base64 的 ASN.1 DER ECDSA）是不是手机对 payload 的签名。审批和指令共用。
func VerifySig(pub *ecdsa.PublicKey, payload []byte, sig string) error {
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) == 0 {
		return ErrBadSignature
	}
	digest := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(pub, digest[:], raw) {
		return ErrBadSignature
	}
	return nil
}

// ParsePublicKey 解析 App 导出的公钥：PEM「PUBLIC KEY」（SubjectPublicKeyInfo），只认 P-256。
func ParsePublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New(`not a PEM "PUBLIC KEY"`)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("public key is not ECDSA P-256")
	}
	return pub, nil
}

// Fingerprint 是给人核对的公钥指纹：SubjectPublicKeyInfo DER 的 SHA-256 前 8 字节，
// 十六进制每 4 位一组，如 "ab12 cd34 ef56 7890"。App 上显示的必须与此一致。
func Fingerprint(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:8])
	return h[0:4] + " " + h[4:8] + " " + h[8:12] + " " + h[12:16], nil
}
