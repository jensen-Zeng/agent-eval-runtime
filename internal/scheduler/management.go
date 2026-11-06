package scheduler

// NotifyCancel interrupts the local worker after an authorized cancellation has
// committed. Durable state remains authoritative if a notification is lost.
func (s *Service) NotifyCancel(id string) { s.cancelActive(id) }
