package adbgateway

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
)

// ParsePublicKey parses the base64 RSAPublicKey portion of an Android ADB .pub file.
// It intentionally does not accept PKIX, PKCS#1 PEM, or arbitrary X.509 keys.
func ParsePublicKey(text string) (*rsa.PublicKey, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return nil, errors.New("invalid ADB public key")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[0])
	const words = 64
	if err != nil || len(decoded) != 4+4+4*words+4*words+4 {
		return nil, errors.New("invalid ADB public key")
	}
	if binary.LittleEndian.Uint32(decoded[:4]) != words {
		return nil, errors.New("unsupported ADB public key size")
	}
	modulusBytes := make([]byte, words*4)
	for i := range modulusBytes {
		modulusBytes[len(modulusBytes)-1-i] = decoded[8+i]
	}
	n := new(big.Int).SetBytes(modulusBytes)
	e := binary.LittleEndian.Uint32(decoded[8+words*4+words*4:])
	if n.BitLen() != 2048 || n.Bit(0) != 1 || e != 65537 {
		return nil, errors.New("unsupported ADB public key")
	}
	return &rsa.PublicKey{N: n, E: int(e)}, nil
}
