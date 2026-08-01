package bot

import "sync"

type step uint8

const (
	stepName step = iota + 1
	stepLanguage
	stepRepo
	stepContributors
	stepConfirm
)

type session struct {
	Step                                                  step
	Name, Language, RepoURL, Owner, RepoName, Description string
	Stars                                                 int
	WantsContributors                                     bool
}
type sessions struct {
	mu   sync.RWMutex
	data map[int64]session
}

func newSessions() *sessions { return &sessions{data: make(map[int64]session)} }
func (s *sessions) get(id int64) (session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[id]
	return v, ok
}
func (s *sessions) set(id int64, v session) { s.mu.Lock(); defer s.mu.Unlock(); s.data[id] = v }
func (s *sessions) delete(id int64)         { s.mu.Lock(); defer s.mu.Unlock(); delete(s.data, id) }
