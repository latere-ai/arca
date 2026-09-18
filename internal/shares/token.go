// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"crypto/rand"
	"encoding/base64"
)

// TokenBytes is the entropy of a link token: 256 bits from crypto/rand,
// which is what makes guessing one cost more than it can pay (spec 015).
const TokenBytes = 32

// TokenLength is the length of the rendered token, base64url with no
// padding: four characters per three bytes, rounded up.
const TokenLength = (TokenBytes*8 + 5) / 6

// newToken mints one capability.
//
// The token is the grantee: whoever holds it reads the subtree the grant
// names, carrying no token of their own. It is rendered base64url rather
// than hex, which is the one change from the service Arca replaces: the
// same entropy in a shorter URL.
//
// crypto/rand.Read does not fail on any supported platform, and a token
// short of its entropy is a capability that can be guessed, so a failure
// here is not something a caller may be handed a weaker token for.
func newToken() string {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("shares: the system random source failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
