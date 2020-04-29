package cluster

import (
	"github.com/lonng/nano/session"
	"sync"
)

type SessionService struct {
	sync.RWMutex
	sessions map[int64]*session.Session //session管理 key:sid,value:Session
}

func NewSessionService() *SessionService {
	return &SessionService{
		sessions: make(map[int64]*session.Session),
	}
}

func (i *SessionService) storeSession(s *session.Session) {
	i.Lock()
	defer i.Unlock()

	i.sessions[s.ID()] = s
}

func (i *SessionService) findSession(sid int64) (*session.Session, bool) {
	i.RLock()
	defer i.RUnlock()
	s, found := i.sessions[sid]
	return s, found
}

func (i *SessionService) remove(sid int64) {
	i.Lock()
	defer i.Unlock()
	delete(i.sessions, sid)
}
