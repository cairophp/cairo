// Package spec holds the server declaration that server.php builds during the
// declare phase. PHP sends it to Go as JSON. This package reads the core's
// part and keeps each extension's part as PHP sent it: only the extension
// knows what its part holds.
package spec

import (
	"encoding/json"
	"fmt"
)

// Spec is everything server.php declared.
type Spec struct {
	Threads        int `json:"threads"`
	OffloadThreads int `json:"offload_threads"`
	// MaxInFlight caps the requests each PHP request thread holds at once
	// (0: no cap). Requests over it wait in Go until one finishes.
	MaxInFlight int `json:"max_in_flight"`
	// Isolated is App::concurrency('isolated'): each request thread runs one
	// request at a time.
	Isolated bool `json:"isolated"`
	// Extensions is what each extension declared, under its name ("http",
	// "grpc", "timing", "blocking"). An extension reads its own with Section.
	Extensions map[string]json.RawMessage `json:"extensions"`
}

// Parse decodes a spec sent by PHP and validates the core's part of it.
func Parse(b []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode server spec: %w", err)
	}
	return &s, s.Validate()
}

// Validate checks the core's part of the spec before anything is served.
func (s *Spec) Validate() error {
	if s.Threads < 0 || s.OffloadThreads < 0 || s.MaxInFlight < 0 {
		return fmt.Errorf("threads must be 'auto' or a positive number")
	}
	return nil
}

// Section decodes into v what the extension called name declared. When it
// declared nothing, v stays as it is.
func (s *Spec) Section(name string, v any) error {
	raw, ok := s.Extensions[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode server spec: %w", err)
	}
	return nil
}
