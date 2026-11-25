package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/decision"
	"net/http"
	"strconv"
)

type jevResultRecord struct {
	Version int          `json:"version"`
	Job     string       `json:"job"`
	Index   int          `json:"index"`
	Hash    string       `json:"hash"`
	Result  jevJobResult `json:"result"`
}

func resultName(job string, index int) string { return fmt.Sprintf("results/%s-%08d.json", job, index) }

// jobResultsPrefix starts the names of every result of job, and of no other job's.
func jobResultsPrefix(id string) string { return "results/" + id + "-" }

func (s *Server) writeJevResult(job jevJob, result jevJobResult) error {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	current, err := s.jevState.readJob(job.ID)
	if err != nil {
		return err
	}
	if current.Cancelled || current.Cursor != job.Cursor {
		return decision.ErrInvalid
	}
	raw, _ := json.Marshal(result)
	return s.jevState.write(resultName(job.ID, job.Cursor), jevResultRecord{Version: 1, Job: job.ID, Index: job.Cursor, Hash: digest(string(raw)), Result: result})
}

func (s *Server) readJevResult(job jevJob, index int) (jevJobResult, error) {
	var record jevResultRecord
	if index < 0 || index >= job.Cursor {
		return record.Result, decision.ErrInvalid
	}
	if err := s.jevState.read(resultName(job.ID, index), &record); err != nil {
		return record.Result, decision.ErrStorage
	}
	return checkJevResult(job, index, record)
}

func checkJevResult(job jevJob, index int, record jevResultRecord) (jevJobResult, error) {
	raw, _ := json.Marshal(record.Result)
	if record.Version != 1 || record.Job != job.ID || record.Index != index || record.Hash != digest(string(raw)) || record.Result.ID != digest(job.ID+fmt.Sprint(index)) || record.Result.Kind != job.Work[index].Kind || record.Result.Status == "" || len(record.Result.Evidence) == 0 {
		return record.Result, decision.ErrStorage
	}
	return record.Result, nil
}

// readJevResults reads every result of job in one query. A result a runner
// wrote before it could advance past it is not yet one of them.
func (s *Server) readJevResults(job jevJob) ([]jevJobResult, error) {
	results := make([]jevJobResult, job.Cursor)
	if job.Cursor == 0 {
		return results, nil
	}
	bodies, err := s.jevState.records.ListBodies(jobResultsPrefix(job.ID), job.Cursor)
	if err != nil {
		return nil, err
	}
	found := 0
	for _, body := range bodies {
		var record jevResultRecord
		if body.Decode(&record) != nil || body.Name != resultName(job.ID, record.Index) {
			return nil, decision.ErrStorage
		}
		if record.Index < 0 || record.Index >= job.Cursor {
			continue
		}
		if results[record.Index], err = checkJevResult(job, record.Index, record); err != nil {
			return nil, err
		}
		found++
	}
	if found != job.Cursor {
		return nil, decision.ErrStorage
	}
	return results, nil
}

func jevPage(r *http.Request) (int, int, error) {
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
func (s *Server) jevResultPage(job jevJob, offset, limit int) ([]jevJobResult, int, error) {
	return jevResultPageOf(job, offset, limit, func(index int) (jevJobResult, error) { return s.readJevResult(job, index) })
}

// jevResultPageOf pages job's results as read returns them by index.
func jevResultPageOf(job jevJob, offset, limit int, read func(int) (jevJobResult, error)) ([]jevJobResult, int, error) {
	if job.Hidden {
		return []jevJobResult{}, 0, nil
	}
	if job.Kind != "audit" {
		total := job.Cursor
		results := []jevJobResult{}
		start, end := jevPageBounds(total, offset, limit)
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
	results := []jevJobResult{}
	start, end := jevPageBounds(len(order), offset, limit)
	for _, source := range order[start:end] {
		partial := job
		partial.Results = []jevJobResult{}
		for _, index := range groups[source] {
			result, err := read(index)
			if err != nil {
				return nil, 0, err
			}
			partial.Results = append(partial.Results, result)
		}
		results = append(results, jevJobResults(partial)...)
	}
	return results, len(order), nil
}

func (s *Server) jevJobPage(job jevJob, offset, limit int) (map[string]any, error) {
	results, total, err := s.jevResultPage(job, offset, limit)
	if err != nil {
		return nil, err
	}
	value := publicJevJob(job)
	value["results"] = results
	value["resultOffset"] = offset
	value["resultTotal"] = total
	value["resultNext"] = nil
	if offset+len(results) < total {
		value["resultNext"] = offset + len(results)
	}
	return value, nil
}

func jevPageBounds(total, offset, limit int) (int, int) {
	start := min(offset, total)
	return start, start + min(limit, total-start)
}
