package workspace

func (s *Store) watchChanges() (<-chan struct{}, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchers == nil {
		s.watchers = map[chan struct{}]struct{}{}
	}
	if len(s.watchers) >= 128 {
		return nil, nil, capacityReached("project subscription limit reached")
	}
	ch := make(chan struct{}, 1)
	s.watchers[ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
	}
	return ch, cancel, nil
}
func (s *Store) changed(revision string) {
	if revision == s.observedHead {
		return
	}
	s.observedHead = revision
	s.notifyLive("")
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
