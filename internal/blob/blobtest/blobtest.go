// Package blobtest serves an in-memory S3 stand-in for tests. It accepts only
// requests signed with its credentials over the payload they carry, as S3
// does, and stores objects path-style under one bucket.
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	Bucket = "forgetmenote-test"
	Region = "us-east-1"
)

// Credentials are the only ones the stand-in accepts.
var Credentials = aws.Credentials{AccessKeyID: "AKIATESTBLOBSTORE", SecretAccessKey: "test-secret-key"}

// S3 is a running stand-in.
type S3 struct {
	URL string

	mu       sync.Mutex
	objects  map[string][]byte
	requests int
	fail     func(*http.Request) int
}

// SetFail has fail answer each later request instead of the stand-in when it
// returns a status, such as a throttling error; nil restores normal service.
// A store's background work may be mid-request, so the hook is set under the
// stand-in's lock.
func (s *S3) SetFail(fail func(*http.Request) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

// Start serves a stand-in until t ends.
func Start(t testing.TB) *S3 {
	s := &S3{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	s.URL = server.URL
	return s
}

// Store returns a store over the stand-in under prefix.
func (s *S3) Store(t testing.TB, prefix string) *blob.S3 {
	store, err := blob.NewS3(blob.S3Config{Bucket: Bucket, Prefix: prefix, Region: Region, Endpoint: s.URL, AllowHTTP: true}, staticCredentials{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Objects returns a copy of every stored object by full key.
func (s *S3) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.objects))
	for key, value := range s.objects {
		out[key] = bytes.Clone(value)
	}
	return out
}

// Requests counts the requests the stand-in has answered.
func (s *S3) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *S3) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if code := fail(r); code != 0 {
			fault(w, code, "SlowDown")
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fault(w, 400, "IncompleteBody")
		return
	}
	if !signed(r, body) {
		fault(w, 403, "SignatureDoesNotMatch")
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+Bucket)
	if !ok {
		fault(w, 404, "NoSuchBucket")
		return
	}
	key = strings.TrimPrefix(key, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case key == "":
		fault(w, 405, "MethodNotAllowed")
	case r.Method == http.MethodPut:
		s.objects[key] = body
	case r.Method == http.MethodGet:
		object, ok := s.objects[key]
		if !ok {
			fault(w, 404, "NoSuchKey")
			return
		}
		if spec := r.Header.Get("Range"); spec != "" {
			var first, last int
			if _, err := fmt.Sscanf(spec, "bytes=%d-%d", &first, &last); err != nil || first > last || first >= len(object) {
				fault(w, 416, "InvalidRange")
				return
			}
			last = min(last, len(object)-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(object)))
			w.Header().Set("Content-Length", strconv.Itoa(last-first+1))
			w.WriteHeader(http.StatusPartialContent)
			object = object[first : last+1]
		}
		w.Write(object)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		fault(w, 405, "MethodNotAllowed")
	}
}

// list pages two keys at a time, so a store must follow continuation tokens.
func (s *S3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) && key > r.URL.Query().Get("continuation-token") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	type content struct {
		Key string `xml:"Key"`
	}
	page := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		Contents              []content `xml:"Contents"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	}{}
	for i, key := range keys {
		if i == 2 {
			page.IsTruncated, page.NextContinuationToken = true, keys[1]
			break
		}
		page.Contents = append(page.Contents, content{key})
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(page)
}

// signed checks r's signature by signing the same request again, and its
// payload hash against body.
func signed(r *http.Request, body []byte) bool {
	sum := sha256.Sum256(body)
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload != hex.EncodeToString(sum[:]) {
		return false
	}
	authorization := r.Header.Get("Authorization")
	_, fields, ok := strings.Cut(authorization, "AWS4-HMAC-SHA256 ")
	if !ok {
		return false
	}
	var headers []string
	for _, field := range strings.Split(fields, ", ") {
		if value, ok := strings.CutPrefix(field, "SignedHeaders="); ok {
			headers = strings.Split(value, ";")
		}
	}
	date, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || len(headers) == 0 {
		return false
	}
	again, err := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), nil)
	if err != nil {
		return false
	}
	again.ContentLength = r.ContentLength
	for _, name := range headers {
		if name != "host" && name != "content-length" {
			again.Header[http.CanonicalHeaderKey(name)] = r.Header.Values(name)
		}
	}
	again.Header.Del("Authorization")
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if signer.SignHTTP(context.Background(), Credentials, again, payload, "s3", Region, date) != nil {
		return false
	}
	return again.Header.Get("Authorization") == authorization
}

func fault(w http.ResponseWriter, code int, name string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	io.WriteString(w, "<Error><Code>"+name+"</Code><Message>stand-in</Message></Error>")
}

type staticCredentials struct{}

func (staticCredentials) Retrieve(context.Context) (aws.Credentials, error) { return Credentials, nil }
