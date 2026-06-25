package notification

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	return newPrefixedID("ntf_")
}

func newAttemptID() string {
	return newPrefixedID("atm_")
}

func newPrefixedID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("generate id: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}
