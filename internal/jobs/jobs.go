// Package jobs owns the single in-memory job and its admission lease.
package jobs

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"

	"csv-cleanroom/internal/csvdata"
)

var ErrBusy = errors.New("busy")
var ErrInvalid = errors.New("invalid_or_expired_job")
var ErrClosed = errors.New("closed")

const Lifetime = 15 * time.Minute

type Snapshot struct {
	Data    *csvdata.Dataset
	Report  *csvdata.Report
	Preview csvdata.Preview
}
type Job struct {
	Original *csvdata.Dataset
	Filename string
	Snapshot *Snapshot
	token    string
	expiry   time.Time
}
type Manager struct {
	mu           sync.Mutex
	busy, closed bool
	job          *Job
	now          func() time.Time
	timer        *time.Timer
}
type Lease struct {
	m     *Manager
	Job   *Job
	ended bool
}

func New(now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{now: now}
}
func (m *Manager) expireLocked() {
	if m.job != nil && !m.now().Before(m.job.expiry) {
		m.job = nil
	}
}
func (m *Manager) scheduleLocked() {
	if m.timer != nil {
		m.timer.Stop()
	}
	if m.job == nil || m.closed {
		return
	}
	d := m.job.expiry.Sub(m.now())
	if d < 0 {
		d = 0
	}
	m.timer = time.AfterFunc(d, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.busy {
			m.expireLocked()
			if m.job != nil {
				m.scheduleLocked()
			}
		}
	})
}

// Acquire checks expiry and token atomically with admission. Uploads do not require a token.
func (m *Manager) Acquire(upload bool, token string) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if m.busy {
		return nil, ErrBusy
	}
	m.expireLocked()
	if !upload && (m.job == nil || len(token) != 43 || subtle.ConstantTimeCompare([]byte(token), []byte(m.job.token)) != 1) {
		return nil, ErrInvalid
	}
	m.busy = true
	return &Lease{m: m, Job: m.job}, nil
}

// CommitUpload replaces the current job after all validation has succeeded.
func (l *Lease) CommitUpload(original *csvdata.Dataset, filename string) (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	m := l.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || l.ended {
		return "", ErrClosed
	}
	m.job = &Job{Original: original, Filename: filename, token: token, expiry: m.now().Add(Lifetime)}
	l.Job = m.job
	return token, nil
}
func (l *Lease) Publish(s *Snapshot) error {
	m := l.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || l.ended || m.job != l.Job {
		return ErrClosed
	}
	m.job.Snapshot = s
	return nil
}

// End releases the gate and refreshes only a completed successful operation.
func (l *Lease) End(success bool) {
	m := l.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	if !m.closed && success && m.job != nil && m.job == l.Job {
		m.job.expiry = m.now().Add(Lifetime)
	}
	m.busy = false
	if !m.closed {
		m.expireLocked()
		m.scheduleLocked()
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.job = nil
	if m.timer != nil {
		m.timer.Stop()
	}
}
