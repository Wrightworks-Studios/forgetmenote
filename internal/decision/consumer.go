package decision

// DecodeRecord applies the same exact typed-field and duplicate-key rules to
// gateway-owned sidecars as to provider answers and the usage ledger.
func DecodeRecord(raw []byte, value any) bool { return strictJSON(raw, value) }

// ValidateRequest lets concrete consumers form complete, bounded batches before
// reserving paid work. It does not read credentials or make a request.
func (s *Service) ValidateRequest(request Request) error {
	if s == nil {
		return ErrDisabled
	}
	_, _, err := s.provider.prepare(request)
	return err
}
func (s *Service) Model() string {
	if s == nil {
		return ""
	}
	return s.config.Model
}

func (s *Service) QuestionLimit() int {
	if s == nil {
		return 0
	}
	return s.config.Limits.Questions
}

func (s *Service) Concurrency() int {
	if s == nil {
		return 0
	}
	return s.config.Limits.Concurrent
}
