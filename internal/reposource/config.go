// Package reposource owns read-only upstream repositories and retained code evidence.
package reposource

import (
	"errors"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/jsonfile"
	"math"
	"regexp"
	"time"
)

var ErrUnavailable = githubapp.ErrUnavailable
var ErrDenied = githubapp.ErrDenied
var ErrConflict = errors.New("repository source changed; refresh and retry")
var ErrBusy = errors.New("repository work is already running; its status will update when it finishes")
var ErrLimit = errors.New("repository source limit reached")
var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Limits struct {
	History       int   `json:"history"`
	CacheBytes    int64 `json:"cacheBytes"`
	EvidenceBytes int64 `json:"evidenceBytes"`
	FileBytes     int64 `json:"fileBytes"`
	Files         int   `json:"files"`
	Seconds       int   `json:"seconds"`
}

type Config struct {
	AppID             string `json:"appId"`
	Slug              string `json:"slug"`
	ClientID          string `json:"clientId"`
	ClientSecretFile  string `json:"clientSecretFile"`
	PrivateKeyFile    string `json:"privateKeyFile"`
	WebhookSecretFile string `json:"webhookSecretFile"`
	Limits            Limits `json:"limits"`
}

func LoadConfig(file string) (Config, error) {
	var c Config
	if err := jsonfile.Load(file, 65536, &c); err != nil {
		return c, errors.New("invalid repository app configuration")
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.AppID == "" || !identifier.MatchString(c.Slug) || c.ClientID == "" || c.ClientSecretFile == "" || c.PrivateKeyFile == "" || c.WebhookSecretFile == "" {
		return errors.New("repository app requires appId, slug, clientId, and mounted secret files")
	}
	l := &c.Limits
	if l.History == 0 {
		l.History = 100
	}
	if l.CacheBytes == 0 {
		l.CacheBytes = 512 << 20
	}
	if l.EvidenceBytes == 0 {
		l.EvidenceBytes = 256 << 20
	}
	if l.FileBytes == 0 {
		l.FileBytes = 1 << 20
	}
	if l.Files == 0 {
		l.Files = 10000
	}
	if l.Seconds == 0 {
		l.Seconds = 120
	}
	if l.History < 1 || l.CacheBytes < 1<<20 || l.EvidenceBytes < 1<<20 || l.FileBytes < 1024 || l.Files < 1 || l.Seconds < 1 || int64(l.Seconds) > math.MaxInt64/int64(time.Second) || l.CacheBytes > math.MaxInt64/64 || l.EvidenceBytes > math.MaxInt64/64 || l.FileBytes > math.MaxInt64/64 {
		return errors.New("repository app limits are outside supported bounds")
	}
	return nil
}
