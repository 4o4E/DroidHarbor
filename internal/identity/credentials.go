// Package identity 使用高熵随机凭证；管理 token 来自 .env，运行态和数据库仅保存摘要。
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

type AdminCredential struct {
	Version string `json:"version"`
	Hash    string `json:"hash"`
}

func Secret() string {
	var entropy [32]byte
	rand.Read(entropy[:])
	return hex.EncodeToString(entropy[:])
}
func Digest(secret string) []byte { digest := sha256.Sum256([]byte(secret)); return digest[:] }

func (credential AdminCredential) Matches(secret string) bool {
	expected, err := hex.DecodeString(credential.Hash)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, Digest(secret)) == 1
}
