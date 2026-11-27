package judgments

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"net/http"
	"strconv"
)

type judgmentResultRecord struct {
	Version int               `json:"version"`
	Job     string            `json:"job"`
	Index   int               `json:"index"`
	Hash    string            `json:"hash"`
	Result  judgmentJobResult `json:"result"`
}

func resultName(job string, index int) string { return fmt.Sprintf("results/%s-%08d.json", job, index) }

// jobResultsPrefix starts the names of every result of job, and of no other job's.
func jobResultsPrefix(id string) string { return "results/" + id + "-" }

func (s *Service) writeJudgmentResult(job judgmentJob, result judgmentJobResult) error {
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	current, err := s.judgmentState.readJob(job.ID)
	if err != nil {
		return err
	}
	if current.Cancelled || current.Cursor != job.Cursor {
		return decision.ErrInvalid
	}
	raw, _ := json.Marshal(result)
	return s.judgmentState.write(resultName(job.ID, job.Cursor), judgmentResultRecord{Version: 1, Job: job.ID, Index: job.Cursor, Hash: auth.Digest(string(raw)), Result: result})
}

func (s *Service) readJudgmentResult(job judgmentJob, index int) (judgmentJobResult, error) {
	var record judgmentResultRecord
	if index < 0 || index >= job.Cursor {
		return record.Result, decision.ErrInvalid
	}
	if err := s.judgmentState.read(resultName(job.ID, index), &record); err != nil {
		return record.Result, decision.ErrStorage
	}
	return checkJudgmentResult(job, index, record)
}

func checkJudgmentResult(job judgmentJob, index int, record judgmentResultRecord) (judgmentJobResult, error) {
	raw, _ := json.Marshal(record.Result)
	if record.Version != 1 || record.Job != job.ID || record.Index != index || record.Hash != auth.Digest(string(raw)) || record.Result.ID != auth.Digest(job.ID+fmt.Sprint(index)) || record.Result.Kind != job.Work[index].Kind || record.Result.Status == "" || len(record.Result.Evidence) == 0 {
		return record.Result, decision.ErrStorage
	}
	return record.Result, nil
}

// readJudgmentResults reads every result of job in one query. A result a runner
// wrote before it could advance past it is not yet one of them.
func (s *Service) readJudgmentResults(job judgmentJob) ([]judgmentJobResult, error) {
	results := make([]judgmentJobResult, job.Cursor)
	if job.Cursor == 0 {
		return results, nil
	}
	bodies, err := s.judgmentState.records.ListBodies(jobResultsPrefix(job.ID), job.Cursor)
	if err != nil {
		return nil, err
	}
	found := 0
	for _, body := range bodies {
		var record judgmentResultRecord
		if body.Decode(&record) != nil || body.Name != resultName(job.ID, record.Index) {
			return nil, decision.ErrStorage
		}
		if record.Index < 0 || record.Index >= job.Cursor {
			continue
		}
		if results[record.Index], err = checkJudgmentResult(job, record.Index, record); err != nil {
			return nil, err
		}
		found++
	}
	if found != job.Cursor {
		return nil, decision.ErrStorage
	}
	return results, nil
}

func judgmentPage(r *http.Request) (int, int, error) {
	offset, limit := 0, 50
	var err error
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, decision.ErrInvalid
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, decision.ErrInvalid
		}
	}
	return offset, limit, nil
}

// Results are individually durable before Cursor advances. Paging reads only the
// requested results; an interrupted write cannot advertise nonexistent output.
func (s *Service) judgmentResultPage(job judgmentJob, offset, limit int) ([]judgmentJobResult, int, error) {
	return judgmentResultPageOf(job, offset, limit, func(index int) (judgmentJobResult, error) { return s.readJudgmentResult(job, index) })
}

// judgmentResultPageOf pages job's results as read returns them by index.
func judgmentResultPageOf(job judgmentJob, offset, limit int, read func(int) (judgmentJobResult, error)) ([]judgmentJobResult, int, error) {
	if job.Hidden {
		return []judgmentJobResult{}, 0, nil
	}
	if job.Kind != "audit" {
		total := job.Cursor
		results := []judgmentJobResult{}
		start, end := judgmentPageBounds(total, offset, limit)
		for i := start; i < end; i++ {
			result, err := read(i)
			if err != nil {
				return nil, 0, err
			}
			results = append(results, result)
		}
		return results, total, nil
	}
	groups := map[string][]int{}
	order := []string{}
	for i, work := range job.Work[:job.Cursor] {
		source := work.Passages[0]
		if groups[source] == nil {
			order = append(order, source)
		}
		groups[source] = append(groups[source], i)
	}
	results := []judgmentJobResult{}
	start, end := judgmentPageBounds(len(order), offset, limit)
	for _, source := range order[start:end] {
		partial := job
		partial.Results = []judgmentJobResult{}
		for _, index := range groups[source] {
			result, err := read(index)
			if err != nil {
				return nil, 0, err
			}
			partial.Results = append(partial.Results, result)
		}
		results = append(results, judgmentJobResults(partial)...)
	}
	return results, len(order), nil
}

func (s *Service) judgmentJobPage(job judgmentJob, offset, limit int) (map[string]any, error) {
	results, total, err := s.judgmentResultPage(job, offset, limit)
	if err != nil {
		return nil, err
	}
	value := publicJudgmentJob(job)
	value["results"] = results
	value["resultOffset"] = offset
	value["resultTotal"] = total
	value["resultNext"] = nil
	if offset+len(results) < total {
		value["resultNext"] = offset + len(results)
	}
	return value, nil
}

func judgmentPageBounds(total, offset, limit int) (int, int) {
	start := min(offset, total)
	return start, start + min(limit, total-start)
}
