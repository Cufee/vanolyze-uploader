package upload

import (
	"errors"
	"sync"
	"time"
)

// Snapshot is the uploader's state as the tray shows it.
type Snapshot struct {
	Activity string
	// Color is "blue" (working), "green" (up to date), "amber" (waiting for
	// Riot), "red" (an error will be retried) or "grey" (setup).
	Color      string
	Setup      bool
	Server     string
	LogPath    string
	Busy       bool
	Uploaded   int
	Stored     int
	LastUpload time.Time
	NextScan   time.Time
	LastError  string
	Updates    string
}

// Status is shared between the scans and the tray. A nil Status ignores updates.
type Status struct {
	mu      sync.RWMutex
	value   Snapshot
	changed chan struct{}
}

func NewStatus() *Status {
	return &Status{
		changed: make(chan struct{}, 1),
		value: Snapshot{
			Activity: "Starting",
			Color:    "blue",
		},
	}
}

// Changed signals after each update.
func (s *Status) Changed() <-chan struct{} {
	return s.changed
}

func (s *Status) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.value
}

func (s *Status) update(change func(*Snapshot)) {
	if s == nil {
		return
	}

	s.mu.Lock()
	change(&s.value)
	s.mu.Unlock()

	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Status) activity(text string) {
	s.update(func(v *Snapshot) {
		v.Activity = text
		v.Color = "blue"
		v.Busy = true
		v.NextScan = time.Time{}
	})
}

func (s *Status) setup(server, logPath string) {
	s.update(func(v *Snapshot) {
		v.Activity = "Set up"
		v.Color = "grey"
		v.Setup = true
		v.Server = server
		v.LogPath = logPath
	})
}

func (s *Status) ready(logPath string) {
	s.update(func(v *Snapshot) {
		v.Activity = "Starting"
		v.Color = "blue"
		v.Setup = false
		v.LogPath = logPath
	})
}

func (s *Status) archived() {
	s.update(func(v *Snapshot) {
		v.Uploaded++
		v.Stored++
		v.LastUpload = time.Now()
	})
}

func (s *Status) stored(n int) {
	s.update(func(v *Snapshot) {
		v.Stored = n
	})
}

func (s *Status) waiting(next time.Time, err error, errorText string) {
	s.update(func(v *Snapshot) {
		v.Busy = false
		v.NextScan = next
		v.LastError = errorText

		var connect *ConnectError

		switch {
		case err == nil:
			v.Activity = "Up to date"
			v.Color = "green"
		case errors.As(err, &connect):
			v.Activity = "Waiting for Riot"
			v.Color = "amber"
		default:
			v.Activity = "Retrying after error"
			v.Color = "red"
		}
	})
}
