package collect

import (
	"crypto/rand"
	"time"
)

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

func newID(prefix string) string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return prefix + time.Now().Format("150405.000")
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return prefix + string(out)
}
