package app

import (
	"fmt"
	"strings"
	"sync"

	"streamvault/core/domain"
	"streamvault/core/storage"
)

// MemStore implementa Persistence em memória: para testes, demos e UIs sem disco.
type MemStore struct {
	mu     sync.Mutex
	tasks  map[string]*domain.DownloadTask
	order  []string
	resume map[string]storage.ResumeState
	hashes map[string]storage.HashEntry
}

// NewMemStore cria um MemStore vazio.
func NewMemStore() *MemStore {
	return &MemStore{tasks: map[string]*domain.DownloadTask{},
		resume: map[string]storage.ResumeState{}, hashes: map[string]storage.HashEntry{}}
}
func (m *MemStore) Save(t *domain.DownloadTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[t.ID]; !ok {
		m.order = append(m.order, t.ID)
	}
	c := *t
	m.tasks[t.ID] = &c
	return nil
}
func (m *MemStore) Get(id string) (*domain.DownloadTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("não encontrada: %s", id)
	}
	c := *t
	return &c, nil
}
func (m *MemStore) List() ([]*domain.DownloadTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*domain.DownloadTask
	for _, id := range m.order {
		c := *m.tasks[id]
		out = append(out, &c)
	}
	return out, nil
}
func (m *MemStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, id)
	for i, o := range m.order {
		if o == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}
func (m *MemStore) SaveResume(rs storage.ResumeState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resume[rs.TaskID] = rs
	return nil
}
func (m *MemStore) LoadResume(id string) (*storage.ResumeState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs, ok := m.resume[id]
	if !ok {
		return nil, nil
	}
	return &rs, nil
}
func (m *MemStore) UpsertHash(e storage.HashEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hashes[e.SHA256Hex] = e
	return nil
}

func (m *MemStore) FindByHash(sha string) ([]storage.HashEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.hashes[strings.ToLower(sha)]; ok {
		return []storage.HashEntry{e}, nil
	}
	return nil, nil
}
