package webcore

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"regexp"
	"strings"
)

var syntheticAuthorName = regexp.MustCompile(`^User [a-f0-9]{12}$`)

type DiscussionOutput struct {
	workspace.Discussion
	People map[string]string `json:"people"`
}
type HistoryOutput struct {
	workspace.Commit
	AuthorDisplayName string `json:"authorDisplayName"`
}

func PresentDiscussion(c *catalog.Catalog, subject, notebook string, value workspace.Discussion) (DiscussionOutput, error) {
	keys := []catalog.ParticipantIdentity{}
	seen := map[string]bool{}
	add := func(author string) {
		if author != "" && !seen[author] {
			seen[author] = true
			keys = append(keys, catalog.ParticipantIdentity{Subject: author})
		}
	}
	for _, comment := range value.Comments {
		add(comment.Author)
		for _, reply := range comment.Replies {
			add(reply.Author)
		}
	}
	names, err := c.ParticipantNames(subject, notebook, keys)
	if err != nil {
		return DiscussionOutput{}, err
	}
	people := make(map[string]string, len(keys))
	for i, key := range keys {
		if key.Subject != "" {
			people[key.Subject] = names[i]
		}
	}
	return DiscussionOutput{Discussion: value, People: people}, nil
}
func PresentHistory(c *catalog.Catalog, subject, notebook string, value []workspace.Commit) ([]HistoryOutput, error) {
	keys := make([]catalog.ParticipantIdentity, 0, len(value))
	for _, row := range value {
		account := row.UserID
		if account == "" && strings.HasSuffix(row.AuthorEmail, "@users.gitnotes.invalid") {
			account = strings.TrimSuffix(row.AuthorEmail, "@users.gitnotes.invalid")
		}
		keys = append(keys, catalog.ParticipantIdentity{Account: account})
	}
	names, err := c.ParticipantNames(subject, notebook, keys)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryOutput, 0, len(value))
	for i, row := range value {
		name := names[i]
		if keys[i].Account == "" {
			if row.Source == "system" {
				name = "System"
			} else if row.AuthorName != "" && !syntheticAuthorName.MatchString(row.AuthorName) {
				name = row.AuthorName
			} else {
				name = "Former contributor"
			}
		}
		out = append(out, HistoryOutput{Commit: row, AuthorDisplayName: name})
	}
	return out, nil
}
