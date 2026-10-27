package scheduler

func (s *Service) RecordReconcile(removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciledContainers += int64(removed)
	if err != nil {
		s.reconcileErrors++
	}
}
