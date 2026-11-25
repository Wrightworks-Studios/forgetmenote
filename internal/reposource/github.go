package reposource

import "forgetmenote/internal/githubapp"

type Repository = githubapp.Repository
type Choice = githubapp.Choice
type rateLimitError = githubapp.RateLimitError

type GitHub struct {
	*githubapp.Client
	Config Config
}

func NewGitHub(c Config) (*GitHub, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	client, err := githubapp.New(githubapp.Config{AppID: c.AppID, Slug: c.Slug, ClientID: c.ClientID, PrivateKeyFile: c.PrivateKeyFile, ClientSecretFile: c.ClientSecretFile, WebhookSecretFile: c.WebhookSecretFile}, "read")
	if err != nil {
		return nil, err
	}
	return &GitHub{Client: client, Config: c}, nil
}
func validRepo(r Repository) bool { return githubapp.ValidRepo(r) }
