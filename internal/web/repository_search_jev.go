package web

import (
	"context"
	"sort"

	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

func (s *Server) rerankRepositoryResults(ctx context.Context, q reposource.SearchQuery, out *reposource.SearchResult) error {
	judge, localOnly := s.jevConfigured()
	if judge == nil || localOnly || len(out.Results) == 0 {
		return nil
	}
	user := subject(ctx)
	prefs, err := s.decisionPreferences(user)
	if err != nil {
		return err
	}
	if !prefs.Consent || !prefs.Rerank {
		return nil
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		return err
	}
	runID, chatID := "source-search-"+RandomToken(), ""
	if scope := chatScopeFrom(ctx); scope != nil {
		runID = scope.Run.ID
		chatID = scope.Chat
	}
	scores := map[string]float64{}
	// Keep each request within one notebook's independent consent and ledger.
	for _, project := range q.Projects {
		passages := []knowledge.Passage{}
		keys := []string{}
		for _, r := range out.Results[:min(knowledge.RelevanceCandidateLimit, len(out.Results))] {
			if r.Project == project {
				passages = append(passages, knowledge.Passage{Path: r.Path, Heading: r.Symbol, Text: r.Text})
				keys = append(keys, r.Project+"/"+r.ID)
			}
		}
		if len(passages) == 0 {
			continue
		}
		state, err := s.repositories.Status(project)
		if err != nil {
			return err
		}
		if !state.IndexConfig.Jev || !state.IndexConfig.ExternalProcessing {
			continue
		}
		check := func(ctx context.Context) error {
			if err := q.Check(ctx, project); err != nil {
				return err
			}
			if err := s.jevAuthorized(ctx, user, project, "rerank", judge); err != nil {
				return err
			}
			current, e := s.repositories.Status(project)
			if e != nil {
				return e
			}
			coverage := out.Coverage[project]
			if !current.Connected || current.Generation != coverage.Generation || current.ID != coverage.Source || current.Downloaded != coverage.Indexed {
				return workspace.ErrConflict
			}
			paths := []string{}
			for _, result := range out.Results {
				if result.Project == project {
					paths = append(paths, result.Path)
				}
			}
			return s.repositories.AuthorizeProcessing(ctx, project, state.ID, coverage.Indexed, paths)
		}
		values, err := knowledge.ScorePassages(ctx, q.Query, passages, knowledge.JevRanking{
			Service: judge,
			Scope:   decision.Scope{Actor: account.ID, Notebook: project, Feature: "rerank", Run: runID, Chat: chatID, Operation: state.Downloaded},
			Check:   check,
		})
		if err != nil {
			return err
		}
		if values == nil {
			return nil
		}
		for i, value := range values {
			scores[keys[i]] = value
		}
	}
	if len(scores) == 0 {
		return nil
	}
	// Do not reorder candidates from unconsented repositories against AI scores.
	for _, project := range q.Projects {
		positions := []int{}
		ranked := []reposource.SourceResult{}
		for i, r := range out.Results {
			if _, ok := scores[r.Project+"/"+r.ID]; ok && r.Project == project {
				positions = append(positions, i)
				ranked = append(ranked, r)
			}
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			if ranked[i].Exact != ranked[j].Exact {
				return ranked[i].Exact
			}
			return scores[project+"/"+ranked[i].ID] > scores[project+"/"+ranked[j].ID]
		})
		for i, pos := range positions {
			out.Results[pos] = ranked[i]
		}
	}
	out.Reranked = true
	return nil
}
