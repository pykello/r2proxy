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

// classify parses an S3 request into (op, bucket, key). Path-style URLs
// (/<bucket>/<key...>) are the primary form; virtual-hosted style (bucket as a
// subdomain of the endpoint host) is also detected.
func classify(r *http.Request, endpointHost string) S3Request {
	bucket, key := splitBucketKey(r, endpointHost)
	q := r.URL.Query()
	has := func(k string) bool { _, ok := q[k]; return ok }

	var op string
	switch r.Method {
	case http.MethodGet, "":
		switch {
		case bucket == "":
			op = "ListBuckets"
		case key == "":
			switch {
			case has("uploads"):
				op = "ListMultipartUploads"
			case has("location"):
				op = "GetBucketLocation"
			case has("versioning"):
				op = "GetBucketVersioning"
			default:
				op = "ListObjects"
			}
		default:
			switch {
			case has("uploadId"):
				op = "ListParts"
			case has("acl"):
				op = "GetObjectAcl"
			case has("tagging"):
				op = "GetObjectTagging"
			default:
				op = "GetObject"
			}
		}
	case http.MethodPut:
		switch {
		case key == "":
			op = "CreateBucket"
		case has("partNumber") && has("uploadId"):
			op = "UploadPart"
		case r.Header.Get("x-amz-copy-source") != "":
			op = "CopyObject"
		case has("tagging"):
			op = "PutObjectTagging"
		case has("acl"):
			op = "PutObjectAcl"
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
			op = "PostObject"
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

func splitBucketKey(r *http.Request, endpointHost string) (bucket, key string) {
	host := stripPort(hostOf(r))
	if host != "" && endpointHost != "" && host != endpointHost &&
		strings.HasSuffix(host, "."+endpointHost) {
		// Virtual-hosted style: <bucket>.<endpointHost>
		bucket = strings.TrimSuffix(host, "."+endpointHost)
		key = strings.TrimPrefix(r.URL.Path, "/")
		return bucket, key
	}
	// Path-style: /<bucket>/<key>
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

func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}
