package githubapp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func (g *Client) Repository(ctx context.Context, token string, id int64) (Repository, error) {
	var repo Repository
	if id <= 0 {
		return repo, ErrDenied
	}
	err := g.Request(ctx, "GET", fmt.Sprintf("%s/repositories/%d", g.API, id), token, nil, &repo)
	if err == nil && (!ValidRepo(repo) || repo.ID != id) {
		err = ErrDenied
	}
	return repo, err
}
func (g *Client) BackupDestination(ctx context.Context, userToken string, installation, id int64, existing bool) (Repository, error) {
	if g.contents != "write" {
		return Repository{}, ErrDenied
	}
	choices, err := g.Choices(ctx, userToken)
	if err != nil {
		return Repository{}, err
	}
	allowed := false
	for _, choice := range choices {
		if choice.ID == id && choice.Installation == installation {
			allowed = true
		}
	}
	if !allowed {
		return Repository{}, ErrDenied
	}
	repo, err := g.Repository(ctx, userToken, id)
	if err != nil {
		return repo, err
	}
	if !repo.Private || !repo.Permissions.Push {
		return repo, ErrDenied
	}
	token, err := g.Token(ctx, installation, id)
	if err != nil {
		return repo, err
	}
	defer g.Revoke(ctx, token)
	current, err := g.Repository(ctx, token, id)
	if err != nil {
		return repo, err
	}
	if !current.Private || current.FullName != repo.FullName {
		return repo, ErrDenied
	}
	var branches []struct {
		Name string `json:"name"`
	}
	if err = g.Request(ctx, "GET", g.API+"/repos/"+repo.FullName+"/branches?per_page=1", token, nil, &branches); err != nil && !IsStatus(err, 409) {
		return repo, err
	}
	if len(branches) > 0 && !existing {
		return repo, fmt.Errorf("backup destination must be an empty private repository")
	}
	return repo, nil
}
func (g *Client) BackupToken(ctx context.Context, installation int64, repo Repository) (string, error) {
	if g.contents != "write" {
		return "", ErrDenied
	}
	token, err := g.Token(ctx, installation, repo.ID)
	if err != nil {
		return "", err
	}
	if err = g.ValidateBackupToken(ctx, token, installation, repo); err != nil {
		g.Revoke(ctx, token)
		return "", err
	}
	return token, nil
}
func (g *Client) ValidateBackupToken(ctx context.Context, token string, installation int64, repo Repository) error {
	if g.contents != "write" || installation <= 0 || !ValidRepo(repo) {
		return ErrDenied
	}
	jwt, err := g.jwt()
	if err != nil {
		return err
	}
	// Repository metadata can remain readable after GitHub suspends an App.
	// Verify the repository's current installation before accepting its token.
	var access struct {
		ID          int64      `json:"id"`
		SuspendedAt *time.Time `json:"suspended_at"`
		Permissions struct {
			Contents string `json:"contents"`
		} `json:"permissions"`
	}
	if err := g.Request(ctx, "GET", g.API+"/repos/"+repo.FullName+"/installation", jwt, nil, &access); err != nil {
		return err
	}
	if access.ID != installation || access.SuspendedAt != nil || access.Permissions.Contents != "write" {
		return ErrDenied
	}
	current, err := g.Repository(ctx, token, repo.ID)
	if err != nil {
		return err
	}
	if !current.Private || !strings.EqualFold(current.FullName, repo.FullName) {
		return ErrDenied
	}
	return nil
}
func (g *Client) RemoteRevision(ctx context.Context, token string, repo Repository, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.Request(ctx, "GET", g.API+"/repos/"+repo.FullName+"/git/ref/heads/"+url.PathEscape(branch), token, nil, &out)
	if err == nil && !revision.MatchString(out.Object.SHA) {
		err = ErrUnavailable
	}
	return out.Object.SHA, err
}
func (g *Client) Revoke(ctx context.Context, token string) error {
	return g.Request(ctx, "DELETE", g.API+"/installation/token", token, nil, nil)
}

// The marker binds retries after an uncertain response to this creation intent.
// A same-named repository without that marker is never adopted or deleted.
func (g *Client) CreatePrivate(ctx context.Context, userToken, owner, name, marker string) (Repository, error) {
	var repo Repository
	if g.contents != "write" || !repoName.MatchString(owner+"/"+name) || strings.Contains(owner+"/"+name, "..") || len(marker) < 32 {
		return repo, ErrDenied
	}
	address := g.API + "/repos/" + owner + "/" + name
	err := g.Request(ctx, "GET", address, userToken, nil, &repo)
	if err == nil {
		if repo.Private && repo.Description == marker && strings.EqualFold(repo.FullName, owner+"/"+name) {
			return repo, nil
		}
		return Repository{}, fmt.Errorf("that repository name already exists; choose another name")
	}
	if !IsStatus(err, 404) {
		return repo, err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err = g.Request(ctx, "GET", g.API+"/user", userToken, nil, &user); err != nil {
		return repo, err
	}
	endpoint := g.API + "/orgs/" + owner + "/repos"
	if strings.EqualFold(user.Login, owner) {
		endpoint = g.API + "/user/repos"
	}
	err = g.Request(ctx, "POST", endpoint, userToken, map[string]any{"name": name, "private": true, "auto_init": false, "description": marker}, &repo)
	if err != nil {
		return repo, err
	}
	if !ValidRepo(repo) || !repo.Private || !strings.EqualFold(repo.FullName, owner+"/"+name) || repo.Description != marker {
		return Repository{}, ErrDenied
	}
	return repo, nil
}
