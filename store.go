package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ResourceStatus struct {
	Service     string
	ID          string
	PausedUntil time.Time
	UsedSeconds time.Duration
	Cooldown    time.Duration
	Window      time.Duration
}

type ResourceDefaults struct {
	Cooldown time.Duration
	Limit    time.Duration
}

type StateStore struct {
	path string
	mu   sync.Mutex
	data map[string]ResourceStatus
}

func NewStateStore(path string) (*StateStore, error) {
	s := &StateStore{
		path: path,
		data: make(map[string]ResourceStatus),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StateStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	statuses := make(map[string]ResourceStatus)

	if _, err := os.Stat(s.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data = statuses
			return nil
		}
		return err
	}

	file, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Split(line, ";")
		if len(parts) != 6 {
			continue
		}

		pausedUnix, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue
		}

		usedSeconds, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			continue
		}

		cooldownSeconds, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil {
			continue
		}

		windowSeconds, err := strconv.ParseInt(parts[5], 10, 64)
		if err != nil {
			continue
		}

		status := ResourceStatus{
			Service:     parts[0],
			ID:          parts[1],
			PausedUntil: time.Unix(pausedUnix, 0),
			UsedSeconds: time.Duration(usedSeconds) * time.Second,
			Cooldown:    time.Duration(cooldownSeconds) * time.Second,
			Window:      time.Duration(windowSeconds) * time.Second,
		}

		statuses[s.key(status.Service, status.ID)] = status
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	s.data = statuses
	return nil
}

func (s *StateStore) saveLocked() error {
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	file, err := os.Create(s.path)
	if err != nil {
		return err
	}
	defer file.Close()

	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	writer := bufio.NewWriter(file)
	for _, key := range keys {
		status := s.data[key]
		pausedUnix := status.PausedUntil.Unix()
		if status.PausedUntil.IsZero() {
			pausedUnix = 0
		}

		line := fmt.Sprintf("%s;%s;%d;%d;%d;%d\n",
			status.Service,
			status.ID,
			pausedUnix,
			int64(status.UsedSeconds/time.Second),
			int64(status.Cooldown/time.Second),
			int64(status.Window/time.Second),
		)

		if _, err := writer.WriteString(line); err != nil {
			return err
		}
	}

	return writer.Flush()
}

func (s *StateStore) Get(service, id string) ResourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}
	return status
}

func (s *StateStore) List(service string) []ResourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]ResourceStatus, 0)
	for _, status := range s.data {
		if status.Service == service {
			result = append(result, status)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})

	return result
}

func (s *StateStore) Update(service, id string, fn func(*ResourceStatus)) (ResourceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	fn(&status)
	status.Service = service
	status.ID = id
	s.data[key] = status

	if err := s.saveLocked(); err != nil {
		return status, err
	}
	return status, nil
}

func (s *StateStore) Acquire(service, id string, defaults ResourceDefaults, now time.Time) (ResourceStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	changed := !ok

	if status.Cooldown == 0 && defaults.Cooldown > 0 {
		status.Cooldown = defaults.Cooldown
		changed = true
	}

	if status.Window == 0 && defaults.Limit > 0 {
		status.Window = defaults.Limit
		changed = true
	}

	if !status.PausedUntil.IsZero() && now.After(status.PausedUntil) {
		status.PausedUntil = time.Time{}
		status.UsedSeconds = 0
		changed = true
	}

	available := status.PausedUntil.IsZero()

	status.Service = service
	status.ID = id
	s.data[key] = status

	if changed {
		if err := s.saveLocked(); err != nil {
			return status, available, err
		}
	}

	return status, available, nil
}

func (s *StateStore) Release(service, id string, usage time.Duration, defaults ResourceDefaults, success bool, now time.Time) (ResourceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.key(service, id)
	status, ok := s.data[key]
	if !ok {
		status = ResourceStatus{Service: service, ID: id}
	}

	changed := !ok

	if status.Cooldown == 0 && defaults.Cooldown > 0 {
		status.Cooldown = defaults.Cooldown
		changed = true
	}

	if status.Window == 0 && defaults.Limit > 0 {
		status.Window = defaults.Limit
		changed = true
	}

	if !success {
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
			changed = true
		}
	}

	status.Service = service
	status.ID = id
	s.data[key] = status

	if changed {
		if err := s.saveLocked(); err != nil {
			return status, err
		}
	}

	return status, nil
}

func (s *StateStore) key(service, id string) string {
	return service + ":" + id
}
