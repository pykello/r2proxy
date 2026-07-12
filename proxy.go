package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// hopByHop headers must not be forwarded through a proxy.
var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
}

// authStripped headers are auth/signing artifacts we always drop and regenerate.
var authStripped = map[string]bool{
	"authorization":                true,
	"x-amz-date":                   true,
	"x-amz-content-sha256":         true,
	"x-amz-security-token":         true,
	"x-amz-decoded-content-length": true,
	"expect":                       true,
}

// ProxyServer is the data-plane handler: it authenticates the caller against the
// single proxy credential, applies injection, and re-signs requests to R2.
type ProxyServer struct {
	app    *App
	client *http.Client
}

func newProxyServer(app *App) *ProxyServer {
	return &ProxyServer{
		app: app,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          256,
				MaxIdleConnsPerHost:   128,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ForceAttemptHTTP2:     true,
			},
			Timeout: 0,
		},
	}
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check bypass.
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
		return
	}

	// 1. Authenticate against the single proxy credential, verify signature.
	ai, ok := parseAuthorization(r)
	if !ok {
		writeS3Error(w, s3Error{403, "AccessDenied", "Missing or malformed Authorization header", 0})
		return
	}
	if ai.AccessKeyID != p.app.ProxyAccessKeyID {
		writeS3Error(w, s3Error{403, "InvalidAccessKeyId", "The access key id you provided does not exist in our records.", 0})
		return
	}
	if !verifyV4(r, ai, p.app.ProxySecretKey) {
		writeS3Error(w, s3Error{403, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", 0})
		return
	}

	// 2. Classify.
	s3 := classify(r)

	p.app.stats.begin()
	start := time.Now()
	remote := clientIP(r)

	// 3. Error injection.
	dec := p.app.engine.decide(s3.Op, s3.Bucket, s3.Key)
	if dec.Delay > 0 {
		select {
		case <-time.After(dec.Delay):
		case <-r.Context().Done():
			p.app.stats.record(reqRecord{
				Time: start, Method: r.Method, Op: s3.Op, Bucket: s3.Bucket, Key: s3.Key,
				Status: 499, DurationMs: msSince(start), Remote: remote, Err: "client canceled",
			})
			return
		}
	}
	if dec.Inject {
		e := resolveInjectedError(dec.Status, dec.Code, dec.Message, dec.RetryAfter)
		writeS3Error(w, e)
		p.app.stats.record(reqRecord{
			Time: start, Method: r.Method, Op: s3.Op, Bucket: s3.Bucket, Key: s3.Key,
			Status: e.Status, DurationMs: msSince(start), Remote: remote,
			Injected: true, Err: "injected " + e.Code,
		})
		return
	}

	// 4. Forward (re-sign) to upstream.
	status, bytesIn, bytesOut, err := p.forward(w, r, s3)
	rec := reqRecord{
		Time: start, Method: r.Method, Op: s3.Op, Bucket: s3.Bucket, Key: s3.Key,
		Status: status, DurationMs: msSince(start), BytesIn: bytesIn, BytesOut: bytesOut,
		Remote: remote,
	}
	if err != nil {
		rec.Err = err.Error()
		if status == 0 {
			rec.Status = 502
		}
	}
	p.app.stats.record(rec)
}

// forward builds, signs, and sends the upstream request, streaming the response
// back to the client. Returns upstream status and byte counts.
func (p *ProxyServer) forward(w http.ResponseWriter, r *http.Request, s3 S3Request) (status int, bytesIn, bytesOut int64, err error) {
	target := strings.TrimRight(p.app.Endpoint, "/") + r.URL.RequestURI()

	var body io.Reader = r.Body
	chunked := isAWSChunked(r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("Content-Encoding"))
	contentLen := int64(-1)
	if chunked {
		body = newAWSChunkedReader(r.Body)
		if dl := r.Header.Get("X-Amz-Decoded-Content-Length"); dl != "" {
			if n, e := strconv.ParseInt(dl, 10, 64); e == nil {
				contentLen = n
			}
		}
	} else if r.ContentLength >= 0 {
		contentLen = r.ContentLength
	}

	// Count request bytes as we stream them upstream.
	var cr *countingReader
	if body != nil && (contentLen != 0) {
		cr = &countingReader{r: body}
		body = cr
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeS3Error(w, s3Error{502, "InternalError", "proxy: " + err.Error(), 0})
		return 0, 0, 0, err
	}
	outReq.ContentLength = contentLen

	// Copy through headers except hop-by-hop, auth artifacts, and (if we
	// de-chunked) the aws-chunked content-encoding.
	for name, vals := range r.Header {
		l := strings.ToLower(name)
		if hopByHop[l] || authStripped[l] || l == "content-length" {
			continue
		}
		if chunked && l == "content-encoding" {
			// Drop aws-chunked; if there were other encodings we'd need to keep
			// them, but S3 clients only use aws-chunked here.
			continue
		}
		for _, v := range vals {
			outReq.Header.Add(name, v)
		}
	}

	signV4(outReq, p.app.UpstreamKeyID, p.app.UpstreamSecret, regionOr(p.app.Region), "s3", UnsignedPayload, time.Now())

	resp, err := p.client.Do(outReq)
	if err != nil {
		if cr != nil {
			bytesIn = cr.n
		}
		writeS3Error(w, s3Error{502, "InternalError", "upstream error: " + err.Error(), 0})
		return 502, bytesIn, 0, err
	}
	defer resp.Body.Close()

	// Relay response.
	for name, vals := range resp.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	if cr != nil {
		bytesIn = cr.n
	}
	return resp.StatusCode, bytesIn, n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// s3Error is a synthetic error response. Its wire format is byte-for-byte the
// same as a real Cloudflare R2 error: a single-line, minimal XML body (Code +
// Message only — no Resource/RequestId), Content-Type application/xml, an
// optional Retry-After, and Server: cloudflare — so injected errors are
// indistinguishable from ones R2 actually returns.
type s3Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int // seconds; >0 sets a Retry-After header
}

func writeS3Error(w http.ResponseWriter, e s3Error) {
	w.Header().Set("Content-Type", "application/xml")
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.Header().Set("Server", "cloudflare")
	w.WriteHeader(e.Status)
	// Matches R2 exactly: no XML-declaration newline, no trailing newline,
	// no <Resource>/<RequestId>.
	fmt.Fprintf(w,
		`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`,
		xmlEscape(e.Code), xmlEscape(e.Message))
}

// resolveInjectedError fills in R2-accurate defaults for an injected error so
// that even a bare "--status 429" reproduces R2's real same-object throttle.
func resolveInjectedError(status int, code, message string, retryAfter int) s3Error {
	if code == "" {
		code = defaultCode(status)
	}
	if message == "" {
		message = defaultMessage(status)
	}
	if retryAfter == 0 && (status == 429 || status == 503) {
		retryAfter = defaultRetryAfter(status)
	}
	return s3Error{Status: status, Code: code, Message: message, RetryAfter: retryAfter}
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func defaultCode(status int) string {
	switch status {
	case 400:
		return "BadRequest"
	case 403:
		return "AccessDenied"
	case 404:
		return "NoSuchKey"
	case 429:
		// R2 returns 429 with code ServiceUnavailable (not SlowDown/TooManyRequests).
		return "ServiceUnavailable"
	case 500:
		return "InternalError"
	case 503:
		return "ServiceUnavailable"
	case 504:
		return "GatewayTimeout"
	default:
		return "InternalError"
	}
}

// defaultMessage returns the message R2 uses for a given status where known.
func defaultMessage(status int) string {
	switch status {
	case 429:
		// Verified against real R2: concurrent same-object throttle.
		return "Reduce your concurrent request rate for the same object."
	case 503:
		return "Please reduce your request rate."
	case 500:
		return "We encountered an internal error. Please try again."
	case 504:
		return "The gateway timed out."
	case 404:
		return "The specified key does not exist."
	case 403:
		return "Access Denied."
	default:
		return defaultCode(status)
	}
}

// defaultRetryAfter mirrors R2's Retry-After for throttling responses.
func defaultRetryAfter(status int) int {
	switch status {
	case 429:
		return 5 // verified: R2 sends Retry-After: 5
	case 503:
		return 1
	default:
		return 0
	}
}

func regionOr(r string) string {
	if r == "" {
		return "auto"
	}
	return r
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
