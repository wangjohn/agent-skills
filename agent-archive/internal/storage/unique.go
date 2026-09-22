package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func uniqueSetupKey() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failure is extraordinarily unlikely; the process-local
		// address is still unique enough to avoid touching a user's object.
		return fmt.Sprintf(".setup-test/fallback-%p", &random)
	}
	return fmt.Sprintf(".setup-test/%s.json", hex.EncodeToString(random[:]))
}
