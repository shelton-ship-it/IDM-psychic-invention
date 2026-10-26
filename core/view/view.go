// Package view define as representações JSON estáveis de tarefas e eventos,
// partilhadas por TODAS as interfaces (API local do Windows, fachada Android).
// Regra de segurança: a URL de origem sai sempre REDIGIDA (sem query/tokens).
package view

import (
	"streamvault/core/domain"
	"streamvault/core/engine"
)

// Err é o diagnóstico mostrado ao utilizador.
type Err struct {
	Class     string `json:"class"`
	Message   string `json:"message"`
	Action    string `json:"action"`
	Retryable bool   `json:"retryable"`
}

// Task é uma tarefa para a UI.
type Task struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Name       string `json:"name"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Priority   int    `json:"priority"`
	Error      *Err   `json:"error,omitempty"`
}

// Event é um evento em tempo real para a UI.
type Event struct {
	Kind       string  `json:"kind"`
	TaskID     string  `json:"task_id"`
	State      string  `json:"state,omitempty"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	SpeedBps   float64 `json:"speed_bps"`
	Error      *Err    `json:"error,omitempty"`
}

// FromError converte um erro de domínio (nil => nil).
func FromError(e *domain.DownloadError) *Err {
	if e == nil {
		return nil
	}
	return &Err{Class: e.Class.String(), Message: e.Message, Action: e.Action, Retryable: e.Retryable}
}

// FromTask converte uma tarefa.
func FromTask(t *domain.DownloadTask) Task {
	name := t.Source.DisplayName
	var size int64 = -1
	if len(t.Files) > 0 {
		name, size = t.Files[0].Path, t.Files[0].SizeBytes
	}
	if t.State == domain.StateCompleted && t.Progress.BytesTotal > 0 {
		size = t.Progress.BytesTotal
	}
	return Task{ID: t.ID, State: t.State.String(), Kind: string(t.Source.Kind), Source: t.Source.Redacted(),
		Name: name, BytesDone: t.Progress.BytesDone, BytesTotal: size, Priority: t.Priority, Error: FromError(t.Error)}
}

var kindNames = map[engine.EventKind]string{
	engine.EventProgress: "progress", engine.EventStateChanged: "state", engine.EventFileDiscovered: "file",
	engine.EventError: "error", engine.EventCompleted: "completed", engine.EventIntegrityReport: "integrity",
}

// FromEvent converte um evento de motor.
func FromEvent(e engine.Event) Event {
	d := Event{Kind: kindNames[e.Kind], TaskID: e.TaskID, BytesDone: e.Progress.BytesDone,
		BytesTotal: e.Progress.BytesTotal, SpeedBps: e.Progress.SpeedBps, Error: FromError(e.Error)}
	if e.Kind == engine.EventStateChanged {
		d.State = e.State.String()
	}
	return d
}
