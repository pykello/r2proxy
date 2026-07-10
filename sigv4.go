package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UnsignedPayload lets us re-sign requests without buffering/hashing the body.
// R2 (like S3) accepts UNSIGNED-PAYLOAD over any transport.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// signV4 signs an outbound *http.Request with AWS SigV4 (header auth) using the
// given credentials. It sets X-Amz-Date, X-Amz-Content-Sha256 and Authorization
// headers. Only host;x-amz-content-sha256;x-amz-date are signed, which S3/R2
// permit (any additional headers we send are simply left unsigned).
func signV4(req *http.Request, accessKey, secretKey, region, service, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalURI := canonicalURIPath(req.URL.EscapedPath())
	canonicalQuery := canonicalQueryString(req.URL.RawQuery)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := "AWS4-HMAC-SHA256 " +
		"Credential=" + accessKey + "/" + scope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", auth)
}

// canonicalURIPath percent-encodes each path segment per AWS rules while
// preserving the slash separators. S3 does not normalize (no dot-segment
// removal), so we re-encode segment-by-segment on the decoded path.
func canonicalURIPath(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	segments := strings.Split(escapedPath, "/")
	for i, s := range segments {
		segments[i] = awsURIEncode(percentDecode(s), false)
	}
	return strings.Join(segments, "/")
}

// canonicalQueryString sorts params by key and URI-encodes keys and values.
func canonicalQueryString(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		var k, v string
		if eq < 0 {
			k = part
		} else {
			k, v = part[:eq], part[eq+1:]
		}
		pairs = append(pairs, kv{
			k: awsURIEncode(percentDecode(k), true),
			v: awsURIEncode(percentDecode(v), true),
		})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// awsURIEncode encodes per RFC3986, leaving A-Za-z0-9-_.~ unreserved.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0xf])
		}
	}
	return b.String()
}

const hexUpper = "0123456789ABCDEF"

// percentDecode decodes %XX sequences; invalid sequences are left as-is.
func percentDecode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			h, l := fromHex(s[i+1]), fromHex(s[i+2])
			if h >= 0 && l >= 0 {
				b.WriteByte(byte(h<<4 | l))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fromHex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
