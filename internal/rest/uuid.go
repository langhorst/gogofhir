package rest

import (
	"crypto/rand"
	"encoding/hex"
)

// uuidV4 returns a random UUID.
//
// Written out rather than pulled from a dependency: it is sixteen random bytes
// with six bits set, and the server otherwise has no need for a UUID library.
// crypto/rand rather than math/rand because ids appear in URLs, and guessable
// ids are a disclosure risk on a server that later gains authorization.
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it does, the
		// process has no entropy and cannot safely continue.
		panic("rest: no entropy available: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	out := make([]byte, 36)
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out)
}
