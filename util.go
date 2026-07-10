package main

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
)

func itoa(i int) string { return strconv.Itoa(i) }

// randHex returns n random bytes hex-encoded (2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// newProxyAccessKey mimics the shape of an AWS/R2 access key id (32 hex chars).
func newProxyAccessKey() string { return randHex(16) }

// newProxySecret returns a 64-char secret.
func newProxySecret() string { return randHex(32) }

// newToken returns a 48-char admin/tenant token.
func newToken() string { return randHex(24) }

// newTenantID returns a short slug for a tenant.
func newTenantID() string { return "t_" + randHex(6) }

// newRuleID returns a short id for an injection rule.
func newRuleID() string { return "r_" + randHex(5) }
