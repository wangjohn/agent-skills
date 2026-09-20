package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func uniqueSetupKey(prefix string) string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failure is extraordinarily unlikely; the process-local
		// address is still unique enough to avoid touching a user's object.
		return fmt.Sprintf("%s/.setup-test/fallback-%p", trimPrefix(prefix), &random)
	}
	return fmt.Sprintf("%s/.setup-test/%s.json", trimPrefix(prefix), hex.EncodeToString(random[:]))
}

func trimPrefix(prefix string) string {
	for len(prefix) > 0 && prefix[0] == '/' {
		prefix = prefix[1:]
	}
	for len(prefix) > 0 && prefix[len(prefix)-1] == '/' {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}
