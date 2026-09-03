package store

import "sync"

type changeBroker struct {
	mu   sync.Mutex
	next int
	subs map[int]chan struct{}
}

func (s *Store) NotifyChange() {
	if s == nil {
		return
	}
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	for _, ch := range s.changes.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Store) SubscribeChanges() (int, <-chan struct{}) {
	ch := make(chan struct{}, 1)
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	id := s.changes.next
	s.changes.next++
	if s.changes.subs == nil {
		s.changes.subs = make(map[int]chan struct{})
	}
	s.changes.subs[id] = ch
	return id, ch
}

func (s *Store) UnsubscribeChanges(id int) {
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	if ch, ok := s.changes.subs[id]; ok {
		close(ch)
		delete(s.changes.subs, id)
	}
}
