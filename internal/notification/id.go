package notification

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("generate notification id: " + err.Error())
	}
	return "ntf_" + hex.EncodeToString(b[:])
}
