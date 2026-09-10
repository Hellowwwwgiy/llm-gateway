package utils

import (
	"crypto/rand"
	"encoding/hex"
)

// NewRequestID 生成一个短 request_id
func NewRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
