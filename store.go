package main

import (
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

type ResourceStatus struct {
	Service     string        `yaml:"service"`
	ID          string        `yaml:"id"`
	PausedUntil time.Time     `yaml:"paused_until"`
	UsedSeconds time.Duration `yaml:"used"`
	Cooldown    time.Duration `yaml:"cooldown"`
	Window      time.Duration `yaml:"window"`
}

type ResourceDefaults struct {
	Cooldown time.Duration
	Limit    time.Duration
}

type StateStore struct {
	mu       sync.RWMutex
	data     map[string]ResourceStatus
	path     string
	saveChan chan struct{}
}

func NewStateStore(path string) *StateStore {
	s := &StateStore{
		data:     make(map[string]ResourceStatus),
		path:     path,
		saveChan: make(chan struct{}, 1),
	}
	s.load()
	go s.saveLoop()
	return s
}

func (s *StateStore) load() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = yaml.NewDecoder(f).Decode(&s.data)
}

func (s *StateStore) save() {
	select {
	case s.saveChan <- struct{}{}:
	default:
		// Save already pending
	}
}

func (s *StateStore) saveLoop() {
	for range s.saveChan {
		s.mu.RLock()
		data := make(map[string]ResourceStatus, len(s.data))
		for k, v := range s.data {
			data[k] = v
		}
		s.mu.RUnlock()

		f, err := os.Create(s.path)
		if err != nil {
			continue
		}
		yaml.NewEncoder(f).Encode(data)
		f.Close()
	}
}

func (s *StateStore) Get(svc, id string) ResourceStatus {
	s.mu.RLock()
	st := s.data[svc+":"+id]
	s.mu.RUnlock()
	if st.Service == "" {
		return ResourceStatus{Service: svc, ID: id}
	}
	return st
}

func (s *StateStore) Acquire(svc, id string, defs ResourceDefaults, now time.Time) (ResourceStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.data[svc+":"+id]
	if st.Cooldown == 0 && defs.Cooldown > 0 {
		st.Cooldown = defs.Cooldown
	}
	if st.Window == 0 && defs.Limit > 0 {
		st.Window = defs.Limit
	}

	if !st.PausedUntil.IsZero() && now.After(st.PausedUntil) {
		st.PausedUntil = time.Time{}
		st.UsedSeconds = 0
	}

	avail := st.PausedUntil.IsZero()
	s.data[svc+":"+id] = st
	s.save()
	return st, avail, nil
}

func (s *StateStore) Release(svc, id string, usage time.Duration, defs ResourceDefaults, ok bool, now time.Time) (ResourceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.data[svc+":"+id]
	if st.Cooldown == 0 && defs.Cooldown > 0 {
		st.Cooldown = defs.Cooldown
	}
	if st.Window == 0 && defs.Limit > 0 {
		st.Window = defs.Limit
	}

	if !ok {
		cd := st.Cooldown
		if cd == 0 {
			cd = defs.Cooldown
		}
		if cd > 0 {
			st.PausedUntil = now.Add(cd)
		}
		st.UsedSeconds = 0
	} else {
		st.UsedSeconds += usage
		limit := st.Window
		if limit == 0 {
			limit = defs.Limit
		}
		if limit > 0 && st.UsedSeconds >= limit {
			cd := st.Cooldown
			if cd == 0 {
				cd = defs.Cooldown
			}
			if cd > 0 {
				st.PausedUntil = now.Add(cd)
			}
			st.UsedSeconds = 0
		}
	}

	s.data[svc+":"+id] = st
	s.save()
	return st, nil
}

