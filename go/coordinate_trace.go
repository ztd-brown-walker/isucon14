package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Times cover the authenticated handler, excluding authentication and log output.
type coordinateTrace struct {
	response     middleware.WrapResponseWriter
	started      time.Time
	stageStarted time.Time
	currentStage string
	fields       map[string]any
	stages       map[string]float64
}

func newCoordinateTrace(w http.ResponseWriter, r *http.Request) *coordinateTrace {
	now := time.Now()
	return &coordinateTrace{
		response:     middleware.NewWrapResponseWriter(w, r.ProtoMajor),
		started:      now,
		stageStarted: now,
		fields: map[string]any{
			"event":      "chair_coordinate",
			"started_at": now.UTC().Format(time.RFC3339Nano),
			"method":     r.Method,
			"path":       r.URL.Path,
		},
		stages: make(map[string]float64),
	}
}

func (t *coordinateTrace) stage(name string) {
	now := time.Now()
	if t.currentStage != "" {
		t.stages[t.currentStage] += float64(now.Sub(t.stageStarted)) / float64(time.Millisecond)
	}
	t.currentStage = name
	t.stageStarted = now
}

func (t *coordinateTrace) finish() {
	lastStage := t.currentStage
	t.stage("")
	t.fields["duration_ms"] = float64(time.Since(t.started)) / float64(time.Millisecond)
	t.fields["status"] = t.response.Status()
	t.fields["last_stage"] = lastStage
	t.fields["stages_ms"] = t.stages
	if data, err := json.Marshal(t.fields); err == nil {
		log.Print(string(data))
	}
}
