package main

import (
	"net/http"
	"strings"
)

// S3Request describes the parsed intent of an incoming S3 request.
type S3Request struct {
	Op     string // e.g. GetObject, PutObject, ListObjects
	Bucket string
	Key    string
}

// classify parses a path-style S3 request (/<bucket>/<key...>) into
// (op, bucket, key). Only the operations worth error-testing are named; anything
// unusual is labeled by HTTP method.
func classify(r *http.Request) S3Request {
	bucket, key := splitBucketKey(r)
	q := r.URL.Query()
	has := func(k string) bool { _, ok := q[k]; return ok }

	var op string
	switch r.Method {
	case http.MethodGet, "":
		switch {
		case bucket == "":
			op = "ListBuckets"
		case key == "":
			op = "ListObjects"
		default:
			op = "GetObject"
		}
	case http.MethodPut:
		switch {
		case key == "":
			op = "CreateBucket"
		case has("partNumber") && has("uploadId"):
			op = "UploadPart"
		case r.Header.Get("x-amz-copy-source") != "":
			op = "CopyObject"
		default:
			op = "PutObject"
		}
	case http.MethodPost:
		switch {
		case has("uploads"):
			op = "CreateMultipartUpload"
		case has("uploadId"):
			op = "CompleteMultipartUpload"
		case has("delete"):
			op = "DeleteObjects"
		default:
			op = "Post"
		}
	case http.MethodDelete:
		switch {
		case has("uploadId"):
			op = "AbortMultipartUpload"
		case key == "":
			op = "DeleteBucket"
		default:
			op = "DeleteObject"
		}
	case http.MethodHead:
		if key == "" {
			op = "HeadBucket"
		} else {
			op = "HeadObject"
		}
	default:
		op = r.Method
	}
	return S3Request{Op: op, Bucket: bucket, Key: key}
}

// splitBucketKey parses path-style /<bucket>/<key>.
func splitBucketKey(r *http.Request) (bucket, key string) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func hostOf(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return r.URL.Host
}
