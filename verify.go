package main

import (
	"crypto/hmac"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// authInfo is the parsed content of an incoming Authorization header.
type authInfo struct {
	AccessKeyID   string
	Date          string // yyyymmdd from credential scope
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// parseAuthorization extracts SigV4 fields from an incoming request's
// Authorization header. ok is false if the header is absent or malformed.
func parseAuthorization(r *http.Request) (authInfo, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(h, prefix) {
		return authInfo{}, false
	}
	var ai authInfo
	for _, part := range strings.Split(h[len(prefix):], ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			cred := strings.TrimPrefix(part, "Credential=")
			segs := strings.Split(cred, "/")
			if len(segs) < 5 {
				return authInfo{}, false
			}
			ai.AccessKeyID = segs[0]
			ai.Date = segs[1]
			ai.Region = segs[2]
			ai.Service = segs[3]
		case strings.HasPrefix(part, "SignedHeaders="):
			ai.SignedHeaders = strings.Split(strings.TrimPrefix(part, "SignedHeaders="), ";")
		case strings.HasPrefix(part, "Signature="):
			ai.Signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if ai.AccessKeyID == "" || ai.Signature == "" || len(ai.SignedHeaders) == 0 {
		return authInfo{}, false
	}
	return ai, true
}

// verifyV4 recomputes the client's SigV4 signature using the given secret and
// compares it (constant time) against the one they presented. This proves the
// caller holds the tenant's proxy secret — real per-tenant authentication.
//
// It reconstructs the exact canonical request the client signed: their signed
// header set/values, their host, and the payload hash they declared via
// x-amz-content-sha256. The body itself is never needed to verify the request.
func verifyV4(r *http.Request, ai authInfo, secretKey string) bool {
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return false
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = UnsignedPayload
	}

	// Build canonical headers from exactly the signed header list.
	var chBuilder strings.Builder
	for _, name := range ai.SignedHeaders {
		lname := strings.ToLower(name)
		var val string
		switch lname {
		case "host":
			val = hostOf(r)
		case "content-length":
			val = trimAll(r.Header.Get("Content-Length"))
		default:
			val = trimAll(r.Header.Get(name))
		}
		chBuilder.WriteString(lname)
		chBuilder.WriteByte(':')
		chBuilder.WriteString(val)
		chBuilder.WriteByte('\n')
	}
	signedHeaders := strings.Join(lowerAll(ai.SignedHeaders), ";")

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURIPath(r.URL.EscapedPath()),
		canonicalQueryString(r.URL.RawQuery),
		chBuilder.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := ai.Date + "/" + ai.Region + "/" + ai.Service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), ai.Date)
	kRegion := hmacSHA256(kDate, ai.Region)
	kService := hmacSHA256(kRegion, ai.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	expected := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return hmac.Equal([]byte(expected), []byte(ai.Signature))
}

// skewOK checks the request timestamp is within maxSkew of now, blocking replay
// of very old signed requests. Empty/unparseable dates fail.
func skewOK(r *http.Request, maxSkew time.Duration) bool {
	t, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	d := time.Since(t)
	if d < 0 {
		d = -d
	}
	return d <= maxSkew
}

func trimAll(s string) string { return strings.TrimSpace(s) }

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
