// Package identity 使用高熵随机凭证；磁盘和数据库只保存服务端摘要。
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func Load(path string) (AdminCredential, error) {
	var credential AdminCredential
	file, err := os.Open(path)
	if err != nil {
		return credential, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&credential); err != nil {
		return credential, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return credential, fmt.Errorf("管理凭证配置包含尾随内容")
	}
	digest, err := hex.DecodeString(credential.Hash)
	if err != nil || len(digest) != sha256.Size || credential.Version == "" {
		return credential, fmt.Errorf("管理凭证配置无效")
	}
	return credential, nil
}

// Initialize 只创建新文件，拒绝覆盖；令牌只返回给初始化命令调用方。
func Initialize(path string) (secret string, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	secret = Secret()
	credential := AdminCredential{Version: Secret(), Hash: hex.EncodeToString(Digest(secret))}
	err = json.NewEncoder(file).Encode(credential)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return "", err
	}
	return secret, nil
}
