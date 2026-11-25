package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// S3Config names a bucket and the prefix under it that holds one instance's
// objects. Credentials come from the AWS default chain: keys in the
// environment first, then a pod's web identity, then the host's role. The bucket's own
// default encryption, versioning and public access block protect the objects.
type S3Config struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	Region string `json:"region"`
	// Endpoint is an S3-compatible service's base URL, addressed path-style;
	// without it requests go to AWS, addressed by bucket host name.
	Endpoint  string `json:"endpoint,omitempty"`
	AllowHTTP bool   `json:"allowHTTP,omitempty"`
	// RoleARN, when set, is the IAM role the store assumes with the pod's web
	// identity token (EKS IAM roles for service accounts) instead of the
	// default chain, so AWS keys in the environment that other components use,
	// such as a Redshift connection's, never sign blob requests.
	RoleARN string `json:"roleArn,omitempty"`
}

var (
	bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	regionName = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	roleName   = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
)

// webIdentitySession names the store's sessions in CloudTrail.
const webIdentitySession = "forgetmenote-blobs"

func (c S3Config) validate() error {
	if !bucketName.MatchString(c.Bucket) {
		return errors.New("blobs s3 bucket must be a DNS-style name without dots")
	}
	if c.Prefix != "" && !ValidKey(c.Prefix) {
		return errors.New("blobs s3 prefix must be a key without leading or trailing slashes")
	}
	if !regionName.MatchString(c.Region) {
		return errors.New("blobs s3 region is required, such as us-east-1")
	}
	if c.RoleARN != "" && !roleName.MatchString(c.RoleARN) {
		return errors.New("blobs s3 roleArn must be an IAM role ARN")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
			return errors.New("blobs s3 endpoint must be a base URL")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && c.AllowHTTP) {
			return errors.New("blobs s3 endpoint requires https, or http with allowHTTP")
		}
	}
	return nil
}

// S3 keeps objects in a bucket through the S3 REST API, signed with
// Signature Version 4 over each payload's SHA-256.
type S3 struct {
	config      S3Config
	base        url.URL
	pathStyle   bool
	credentials aws.CredentialsProvider
	client      *http.Client
	signer      *v4.Signer
}

// OpenS3 returns the store c names with the AWS default credential chain, or
// as c's role with the pod's web identity token.
func OpenS3(ctx context.Context, c S3Config) (*S3, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region))
	if err != nil {
		return nil, errors.New("cannot load AWS credentials for blob storage")
	}
	if c.RoleARN == "" {
		return NewS3(c, cfg.Credentials, nil)
	}
	token := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if token == "" {
		return nil, errors.New("blobs s3 roleArn needs the pod's web identity token (AWS_WEB_IDENTITY_TOKEN_FILE); give its service account an EKS IAM role")
	}
	// The exchange itself is unsigned, so no other credential is ever sent.
	client := sts.NewFromConfig(cfg, func(o *sts.Options) { o.Credentials = aws.AnonymousCredentials{} })
	role := stscreds.NewWebIdentityRoleProvider(client, c.RoleARN, stscreds.IdentityTokenFile(token),
		func(o *stscreds.WebIdentityRoleOptions) { o.RoleSessionName = webIdentitySession })
	return NewS3(c, aws.NewCredentialsCache(role), nil)
}

// NewS3 returns the store c names with the given credentials, over client or
// a client of its own when client is nil.
func NewS3(c S3Config, credentials aws.CredentialsProvider, client *http.Client) (*S3, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, errors.New("blob storage needs AWS credentials")
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.ResponseHeaderTimeout = time.Minute
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// S3 signs the path as sent rather than escaping it again.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	s := &S3{config: c, credentials: credentials, client: client, signer: signer}
	if c.Endpoint != "" {
		u, _ := url.Parse(c.Endpoint)
		s.base = url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + c.Bucket}
		s.pathStyle = true
	} else {
		s.base = url.URL{Scheme: "https", Host: c.Bucket + ".s3." + c.Region + ".amazonaws.com", Path: ""}
	}
	return s, nil
}

func (s *S3) objectKey(key string) string {
	if s.config.Prefix == "" {
		return key
	}
	return s.config.Prefix + "/" + key
}

func (s *S3) objectURL(key string) string {
	u := s.base
	u.Path += "/" + s.objectKey(key)
	return u.String()
}

const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	if !ValidKey(key) {
		return errors.New("invalid blob key")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, readerContext{ctx, body})
	if err != nil {
		return err
	}
	response, err := s.do(ctx, http.MethodPut, s.objectURL(key), key, body, size, hex.EncodeToString(hash.Sum(nil)))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return s.failure("put", key, response)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, errors.New("invalid blob key")
	}
	response, err := s.do(ctx, http.MethodGet, s.objectURL(key), key, nil, 0, emptyPayload)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		return response.Body, nil
	case http.StatusNotFound:
		response.Body.Close()
		return nil, notFound(key)
	}
	defer response.Body.Close()
	return nil, s.failure("get", key, response)
}

func (s *S3) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, errors.New("invalid blob key")
	}
	if offset < 0 || length < 1 {
		return nil, errors.New("invalid blob range")
	}
	response, err := s.do(ctx, http.MethodGet, s.objectURL(key), key, nil, 0, emptyPayload, fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusPartialContent:
		if response.ContentLength == length {
			return response.Body, nil
		}
	case http.StatusNotFound:
		response.Body.Close()
		return nil, notFound(key)
	case http.StatusRequestedRangeNotSatisfiable:
		response.Body.Close()
		return nil, fmt.Errorf("%s: range beyond the object", key)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPartialContent {
		return nil, fmt.Errorf("get %s: S3 returned a different range", key)
	}
	return nil, s.failure("get", key, response)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid blob key")
	}
	response, err := s.do(ctx, http.MethodDelete, s.objectURL(key), key, nil, 0, emptyPayload)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
		return s.failure("delete", key, response)
	}
	return nil
}

type listing struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	if !validPrefix(prefix) {
		return nil, errors.New("invalid blob prefix")
	}
	full := s.objectKey(prefix)
	var keys []string
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {full}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		u := s.base
		if u.Path == "" {
			u.Path = "/"
		}
		u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
		response, err := s.do(ctx, http.MethodGet, u.String(), prefix, nil, 0, emptyPayload)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			err = s.failure("list", prefix, response)
			response.Body.Close()
			return nil, err
		}
		var page listing
		err = xml.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&page)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: unreadable S3 listing", prefix)
		}
		for _, object := range page.Contents {
			key, ok := strings.CutPrefix(object.Key, strings.TrimSuffix(full, prefix))
			// Objects nobody stored through a store, such as a console upload
			// with another name, are not listed.
			if ok && strings.HasPrefix(key, prefix) && ValidKey(key) {
				keys = append(keys, key)
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

// do signs and sends a request, retrying a throttled or failed attempt with
// backoff; every S3 request it sends is idempotent.
func (s *S3) do(ctx context.Context, method, target, key string, body io.ReadSeeker, size int64, payload string, byteRange ...string) (*http.Response, error) {
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		}
		var reader io.Reader
		if body != nil {
			if _, err := body.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			reader = io.LimitReader(readerContext{ctx, body}, size)
		}
		request, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			request.ContentLength = size
			request.Header.Set("Content-Type", "application/octet-stream")
			if size == 0 {
				request.Body = http.NoBody
			}
		}
		request.Header.Set("X-Amz-Content-Sha256", payload)
		if len(byteRange) > 0 {
			request.Header.Set("Range", byteRange[0])
		}
		credentials, err := s.credentials.Retrieve(ctx)
		if err != nil {
			return nil, errors.New("cannot retrieve AWS credentials for blob storage")
		}
		if err = s.signer.SignHTTP(ctx, credentials, request, payload, "s3", s.config.Region, time.Now()); err != nil {
			return nil, err
		}
		response, err := s.client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = fmt.Errorf("%s %s: S3 is unreachable", strings.ToLower(method), key)
			continue
		}
		switch response.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			last = s.failure(strings.ToLower(method), key, response)
			response.Body.Close()
			continue
		}
		return response, nil
	}
	return nil, last
}

// failure names S3's error code for an operation, never its request or
// credentials.
func (s *S3) failure(operation, key string, response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	var e struct {
		Code string `xml:"Code"`
	}
	if xml.NewDecoder(bytes.NewReader(raw)).Decode(&e) != nil || e.Code == "" {
		e.Code = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("%s %s: S3 returned %d %s", operation, key, response.StatusCode, e.Code)
}
