package main

import (
	"os"
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
	path   string
	data   map[string]ResourceStatus
	saveCh chan struct{}
}

func NewStateStore(path string) (*StateStore, error) {
	s := &StateStore{
		path:   path,
		data:   make(map[string]ResourceStatus),
		saveCh: make(chan struct{}, 1),
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	go s.saveWorker()

	return s, nil
}

func (s *StateStore) saveWorker() {
	for range s.saveCh {
		s.save()
	}
}

func (s *StateStore) load() error {
	s.data = make(map[string]ResourceStatus)
	
	file, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var statuses []ResourceStatus
	if err := yaml.NewDecoder(file).Decode(&statuses); err != nil {
		return nil
	}

	for _, status := range statuses {
		s.data[status.Service+":"+status.ID] = status
	}
	return nil
}

func (s *StateStore) save() {
	file, _ := os.Create(s.path)
	defer file.Close()

	var statuses []ResourceStatus
	for _, status := range s.data {
		statuses = append(statuses, status)
	}

	_ = yaml.NewEncoder(file).Encode(statuses)
}

func (s *StateStore) triggerSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *StateStore) Get(service, id string) ResourceStatus {
	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}
	return status
}

func (s *StateStore) List(service string) []ResourceStatus {
	result := make([]ResourceStatus, 0)
	for _, status := range s.data {
		if status.Service == service {
			result = append(result, status)
		}
	}
	return result
}

func (s *StateStore) applyDefaults(status *ResourceStatus, defaults ResourceDefaults) bool {
	changed := false
	if status.Cooldown == 0 && defaults.Cooldown > 0 {
		status.Cooldown = defaults.Cooldown
		changed = true
	}
	if status.Window == 0 && defaults.Limit > 0 {
		status.Window = defaults.Limit
		changed = true
	}
	return changed
}

func (s *StateStore) applyCooldown(status *ResourceStatus, defaults ResourceDefaults, now time.Time) {
	cooldown := status.Cooldown
	if cooldown == 0 {
		cooldown = defaults.Cooldown
	}
	if cooldown > 0 {
		status.PausedUntil = now.Add(cooldown)
	} else {
		status.PausedUntil = time.Time{}
	}
	status.UsedSeconds = 0
}

func (s *StateStore) normalize(status *ResourceStatus, service, id string) {
	status.Service = service
	status.ID = id
}

func (s *StateStore) Update(service, id string, fn func(*ResourceStatus)) (ResourceStatus, error) {
	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	fn(&status)
	status.Service = service
	status.ID = id
	s.data[key] = status

	s.triggerSave()
	return status, nil
}

func (s *StateStore) Acquire(service, id string, defaults ResourceDefaults, now time.Time) (ResourceStatus, bool, error) {
	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	changed := !ok || s.applyDefaults(&status, defaults)

	if !status.PausedUntil.IsZero() && now.After(status.PausedUntil) {
		status.PausedUntil = time.Time{}
		status.UsedSeconds = 0
		changed = true
	}

	available := status.PausedUntil.IsZero()

	s.normalize(&status, service, id)
	s.data[key] = status

	if changed {
		s.triggerSave()
	}

	return status, available, nil
}

func (s *StateStore) Release(service, id string, usage time.Duration, defaults ResourceDefaults, success bool, now time.Time) (ResourceStatus, error) {
	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	changed := !ok || s.applyDefaults(&status, defaults)

	if !success {
		s.applyCooldown(&status, defaults, now)
		changed = true
	} else {
		if usage > 0 {
			status.UsedSeconds += usage
			changed = true
		}

		limit := status.Window
		if limit == 0 {
			limit = defaults.Limit
		}

		if limit > 0 && status.UsedSeconds >= limit {
			s.applyCooldown(&status, defaults, now)
			changed = true
		}
	}

	s.normalize(&status, service, id)
	s.data[key] = status

	if changed {
		s.triggerSave()
	}

	return status, nil
}

func (s *StateStore) key(service, id string) string {
	return service + ":" + id
}
