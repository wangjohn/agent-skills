package storage

import "crypto/sha256"

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	const hex = "0123456789abcdef"
	result := make([]byte, len(sum)*2)
	for i, b := range sum {
		result[i*2] = hex[b>>4]
		result[i*2+1] = hex[b&15]
	}
	return string(result)
}
