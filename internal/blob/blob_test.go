package blob_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func stores(t *testing.T) map[string]blob.Store {
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	return map[string]blob.Store{"directory": dir, "s3": blobtest.Start(t).Store(t, "instance")}
}

func TestStoresKeepReplaceListAndDeleteObjects(t *testing.T) {
	ctx := context.Background()
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"jobs/b/page-00002.json", "jobs/a/original", "jobs/a/result.json", "jobs-old/x", "jobsx/y", "other/z"} {
				if err := blob.Write(ctx, store, key, []byte("first "+key)); err != nil {
					t.Fatal(key, err)
				}
			}
			if err := blob.Write(ctx, store, "jobs/a/original", []byte("second")); err != nil {
				t.Fatal(err)
			}
			if raw, err := blob.Read(ctx, store, "jobs/a/original", 6); err != nil || string(raw) != "second" {
				t.Fatalf("replaced object: %q %v", raw, err)
			}
			if _, err := blob.Read(ctx, store, "jobs/a/original", 5); !errors.Is(err, blob.ErrTooLarge) {
				t.Fatalf("an object over its limit was read: %v", err)
			}
			if err := blob.Write(ctx, store, "jobs/empty", nil); err != nil {
				t.Fatal(err)
			}
			if raw, err := blob.Read(ctx, store, "jobs/empty", 0); err != nil || len(raw) != 0 {
				t.Fatalf("empty object: %q %v", raw, err)
			}
			keys, err := store.List(ctx, "jobs/")
			if want := []string{"jobs/a/original", "jobs/a/result.json", "jobs/b/page-00002.json", "jobs/empty"}; err != nil || !slices.Equal(keys, want) {
				t.Fatalf("listing: %q %v, want %q", keys, err, want)
			}
			if err = blob.RemoveAll(ctx, store, "jobs/a/"); err != nil {
				t.Fatal(err)
			}
			if _, err = store.Get(ctx, "jobs/a/result.json"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("removed object: %v", err)
			}
			if err = store.Delete(ctx, "jobs/a/result.json"); err != nil {
				t.Fatalf("deleting a missing object: %v", err)
			}
			if keys, err = store.List(ctx, "jobs/a/"); err != nil || len(keys) != 0 {
				t.Fatalf("removed prefix lists %q %v", keys, err)
			}
			if keys, err = store.List(ctx, "missing/"); err != nil || len(keys) != 0 {
				t.Fatalf("missing prefix lists %q %v", keys, err)
			}
			if raw, err := blob.Read(ctx, store, "other/z", 64); err != nil || string(raw) != "first other/z" {
				t.Fatalf("unrelated object: %q %v", raw, err)
			}
		})
	}
}

func TestStoresRefuseKeysOutsideTheirLayout(t *testing.T) {
	ctx := context.Background()
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"", "/a", "a/", "a//b", "../a", "a/../b", "a/.blob-x", ".hidden", "a b", "a\\b", strings.Repeat("a", 513)} {
				if store.Put(ctx, key, bytes.NewReader(nil)) == nil {
					t.Errorf("stored %q", key)
				}
				if _, err := store.Get(ctx, key); err == nil || errors.Is(err, fs.ErrNotExist) {
					t.Errorf("read %q: %v", key, err)
				}
				if store.Delete(ctx, key) == nil {
					t.Errorf("deleted %q", key)
				}
			}
			for _, prefix := range []string{"", "a", "/a/", "../"} {
				if _, err := store.List(ctx, prefix); err == nil {
					t.Errorf("listed %q", prefix)
				}
			}
		})
	}
}

func TestDirectoryWritesInPlaceAndLeavesOnlyObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := blob.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = blob.Write(ctx, store, "source-jobs/0123/original", []byte("original")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "source-jobs", "0123", "original"))
	info, statErr := os.Stat(filepath.Join(root, "source-jobs", "0123", "original"))
	if err != nil || statErr != nil || string(raw) != "original" || info.Mode().Perm() != 0o600 {
		t.Fatalf("the object is not the file at its key: %q %v %v", raw, err, statErr)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "source-jobs", "0123"))
	if len(entries) != 1 {
		t.Fatalf("a write left %d entries", len(entries))
	}
	// A temporary an interrupted write left is never an object.
	os.WriteFile(filepath.Join(root, "source-jobs", "0123", ".blob-interrupted"), []byte("partial"), 0o600)
	if keys, err := store.List(ctx, "source-jobs/"); err != nil || !slices.Equal(keys, []string{"source-jobs/0123/original"}) {
		t.Fatalf("listing: %q %v", keys, err)
	}
	os.Remove(filepath.Join(root, "source-jobs", "0123", ".blob-interrupted"))
	if err = store.Delete(ctx, "source-jobs/0123/original"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "source-jobs", "0123")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the emptied directory remains: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "source-jobs")); err != nil {
		t.Fatalf("the area directory was removed: %v", err)
	}
}

// Delete removes the directories it empties, so a listing can find a directory
// gone that its parent still named. Such a directory held no objects to list.
func TestDirectoryListsWhileObjectsAreDeleted(t *testing.T) {
	ctx := context.Background()
	store, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var keys []string
	for i := range 300 {
		key := fmt.Sprintf("source-jobs/%04d/original", i)
		if err = blob.Write(ctx, store, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	deleted := make(chan error, 1)
	go func() {
		for _, key := range keys {
			if err := store.Delete(ctx, key); err != nil {
				deleted <- err
				return
			}
		}
		deleted <- nil
	}()
	for done := false; !done; {
		select {
		case err := <-deleted:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		default:
		}
		listed, err := store.List(ctx, "source-jobs/")
		if err != nil {
			t.Fatalf("a listing failed while objects were deleted: %v", err)
		}
		for _, key := range listed {
			if !slices.Contains(keys, key) {
				t.Fatalf("listed %q, which was never stored", key)
			}
		}
	}
	if listed, err := store.List(ctx, "source-jobs/"); err != nil || len(listed) != 0 {
		t.Fatalf("after every delete: %q %v", listed, err)
	}
}

func TestDirectoryRefusesLinksOutOfItsRoot(t *testing.T) {
	ctx := context.Background()
	root, outside := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "area")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	store, err := blob.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = blob.Read(ctx, store, "area/secret", 64); err == nil {
		t.Fatal("read through a link out of the root")
	}
	if _, err = blob.Read(ctx, store, "linked", 64); err == nil {
		t.Fatal("read a linked object")
	}
	if blob.Write(ctx, store, "area/written", []byte("x")) == nil {
		t.Fatal("wrote through a link out of the root")
	}
	if _, err = os.Stat(filepath.Join(outside, "written")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a write landed outside the root")
	}
}

type otherCredentials struct{}

func (otherCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: blobtest.Credentials.AccessKeyID, SecretAccessKey: "another-secret"}, nil
}

func TestS3SignsEachRequestOverItsPayload(t *testing.T) {
	ctx := context.Background()
	s3 := blobtest.Start(t)
	if err := blob.Write(ctx, s3.Store(t, ""), "area/object", []byte("signed")); err != nil {
		t.Fatal(err)
	}
	forged, err := blob.NewS3(blob.S3Config{Bucket: blobtest.Bucket, Region: blobtest.Region, Endpoint: s3.URL, AllowHTTP: true}, otherCredentials{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = blob.Write(ctx, forged, "area/object", []byte("forged"))
	if err == nil || !strings.Contains(err.Error(), "403 SignatureDoesNotMatch") || strings.Contains(err.Error(), "another-secret") {
		t.Fatalf("a request under other credentials: %v", err)
	}
	if objects := s3.Objects(); string(objects["area/object"]) != "signed" {
		t.Fatalf("objects: %q", objects)
	}
}

func TestS3KeepsEachInstanceUnderItsPrefix(t *testing.T) {
	ctx := context.Background()
	s3 := blobtest.Start(t)
	first, second := s3.Store(t, "cells/first"), s3.Store(t, "cells/second")
	blob.Write(ctx, first, "jobs/a", []byte("first"))
	blob.Write(ctx, second, "jobs/a", []byte("second"))
	if keys, err := first.List(ctx, "jobs/"); err != nil || !slices.Equal(keys, []string{"jobs/a"}) {
		t.Fatalf("listing: %q %v", keys, err)
	}
	if raw, err := blob.Read(ctx, second, "jobs/a", 64); err != nil || string(raw) != "second" {
		t.Fatalf("second instance: %q %v", raw, err)
	}
	objects := s3.Objects()
	if len(objects) != 2 || string(objects["cells/first/jobs/a"]) != "first" || string(objects["cells/second/jobs/a"]) != "second" {
		t.Fatalf("objects: %q", objects)
	}
}

func TestS3RetriesThrottledRequests(t *testing.T) {
	ctx := context.Background()
	s3 := blobtest.Start(t)
	store := s3.Store(t, "")
	failures := 2
	s3.SetFail(func(*http.Request) int {
		if failures > 0 {
			failures--
			return http.StatusServiceUnavailable
		}
		return 0
	})
	body := bytes.NewReader([]byte("retried payload"))
	io.ReadAll(body) // Put reads from the start, wherever the reader stands.
	if err := store.Put(ctx, "area/object", body); err != nil {
		t.Fatal(err)
	}
	if raw, err := blob.Read(ctx, store, "area/object", 64); err != nil || string(raw) != "retried payload" || s3.Requests() != 4 {
		t.Fatalf("retried object: %q %v after %d requests", raw, err, s3.Requests())
	}
	s3.SetFail(func(*http.Request) int { return http.StatusServiceUnavailable })
	if err := blob.Write(ctx, store, "area/object", []byte("x")); err == nil || !strings.Contains(err.Error(), "503 SlowDown") {
		t.Fatalf("a store that keeps failing: %v", err)
	}
}

// An instance whose environment holds another component's AWS keys, such as a
// Redshift connection's, still signs blob requests as its own role, assumed
// with the pod's web identity token.
func TestS3AssumesItsRoleWithThePodsWebIdentity(t *testing.T) {
	ctx := context.Background()
	s3 := blobtest.Start(t)
	var mu sync.Mutex
	var calls []url.Values
	signedCalls := 0
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		calls = append(calls, r.PostForm)
		if r.Header.Get("Authorization") != "" {
			signedCalls++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>web-identity-session</SessionToken><Expiration>%s</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/blobs/forgetmenote-blobs</Arn><AssumedRoleId>AROATESTBLOBS:forgetmenote-blobs</AssumedRoleId></AssumedRoleUser></AssumeRoleWithWebIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></AssumeRoleWithWebIdentityResponse>`,
			blobtest.Credentials.AccessKeyID, blobtest.Credentials.SecretAccessKey, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(sts.Close)
	home := t.TempDir()
	token := filepath.Join(home, "token")
	if err := os.WriteFile(token, []byte("pod-identity-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":           "AKIAOTHERCOMPONENT",
		"AWS_SECRET_ACCESS_KEY":       "other-component-secret",
		"AWS_ROLE_ARN":                "arn:aws:iam::123456789012:role/tenant",
		"AWS_WEB_IDENTITY_TOKEN_FILE": token,
		"AWS_ENDPOINT_URL_STS":        sts.URL,
		"AWS_CONFIG_FILE":             filepath.Join(home, "config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(home, "credentials"),
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_PROFILE":                 "",
	} {
		t.Setenv(name, value)
	}
	config := blob.S3Config{Bucket: blobtest.Bucket, Region: blobtest.Region, Endpoint: s3.URL, AllowHTTP: true}
	chained, err := blob.OpenS3(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = blob.Write(ctx, chained, "area/object", []byte("as the environment")); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the default chain signs with the environment's keys: %v", err)
	}
	config.RoleARN = "arn:aws:iam::123456789012:role/blobs"
	store, err := blob.OpenS3(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = blob.Write(ctx, store, "area/object", []byte("as the role")); err != nil {
		t.Fatal(err)
	}
	if objects := s3.Objects(); string(objects["area/object"]) != "as the role" {
		t.Fatalf("objects: %q", objects)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0].Get("Action") != "AssumeRoleWithWebIdentity" || calls[0].Get("RoleArn") != config.RoleARN ||
		calls[0].Get("WebIdentityToken") != "pod-identity-token" || calls[0].Get("RoleSessionName") != "forgetmenote-blobs" || signedCalls != 0 {
		t.Fatalf("STS calls %v, %d signed", calls, signedCalls)
	}
	mu.Unlock()
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	if _, err = blob.OpenS3(ctx, config); err == nil || !strings.Contains(err.Error(), "web identity") {
		t.Fatalf("a role without the pod's token: %v", err)
	}
}

func TestConfigNamesOneStore(t *testing.T) {
	valid := []blob.Config{
		{},
		{Directory: "/var/lib/forgetmenote/blobs"},
		{S3: &blob.S3Config{Bucket: "notes-blobs", Prefix: "instance", Region: "us-east-1"}},
		{S3: &blob.S3Config{Bucket: "local-test", Region: "us-east-1", Endpoint: "http://s3:9000", AllowHTTP: true}},
		{S3: &blob.S3Config{Bucket: "notes-blobs", Region: "us-east-1", RoleARN: "arn:aws:iam::123456789012:role/forgetmenote-blobs"}},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	invalid := []blob.Config{
		{Directory: "relative"},
		{Directory: "/data", S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1"}},
		{S3: &blob.S3Config{Bucket: "Bucket", Region: "us-east-1"}},
		{S3: &blob.S3Config{Bucket: "dotted.bucket", Region: "us-east-1"}},
		{S3: &blob.S3Config{Bucket: "bucket"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", Prefix: "/leading"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", Prefix: "trailing/"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", Endpoint: "http://s3:9000"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", Endpoint: "https://user:pass@s3"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", Endpoint: "https://s3/path"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", RoleARN: "forgetmenote-blobs"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", RoleARN: "arn:aws:iam::7803:role/blobs"}},
		{S3: &blob.S3Config{Bucket: "bucket", Region: "us-east-1", RoleARN: "arn:aws:iam::123456789012:user/reporting-api"}},
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}

func TestStoresReadARangeWithinAnObject(t *testing.T) {
	ctx := context.Background()
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			blob.Write(ctx, store, "exports/bundle", []byte("0123456789"))
			read := func(offset, length int64) (string, error) {
				r, err := store.GetRange(ctx, "exports/bundle", offset, length)
				if err != nil {
					return "", err
				}
				defer r.Close()
				raw, err := io.ReadAll(r)
				return string(raw), err
			}
			for _, c := range []struct {
				offset, length int64
				want           string
			}{{0, 10, "0123456789"}, {3, 4, "3456"}, {9, 1, "9"}} {
				if got, err := read(c.offset, c.length); err != nil || got != c.want {
					t.Errorf("range %d+%d: %q %v", c.offset, c.length, got, err)
				}
			}
			for _, c := range [][2]int64{{8, 3}, {10, 1}, {-1, 2}, {0, 0}} {
				if got, err := read(c[0], c[1]); err == nil {
					t.Errorf("range %d+%d beyond the object read %q", c[0], c[1], got)
				}
			}
			if _, err := store.GetRange(ctx, "exports/missing", 0, 1); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("missing object: %v", err)
			}
		})
	}
}
