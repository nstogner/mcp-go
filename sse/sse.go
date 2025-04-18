package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/riza-io/mcp-go"
)

type session struct {
	out chan *mcp.Message
}

type message struct {
	sessionID string
	msg       *mcp.Message
}

type Stream struct {
	mu            sync.RWMutex
	in            chan *mcp.Message
	sessions      map[string]*session
	messagesRoute string
	sseRoute      string
}

func writeEvent(w http.ResponseWriter, id string, event string, data string) {
	fmt.Fprintf(w, "id: %s\n", id)
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func NewStream(sseRoute, messagesRoute string) *Stream {
	s := &Stream{
		in:            make(chan *mcp.Message),
		sessions:      make(map[string]*session),
		messagesRoute: messagesRoute,
		sseRoute:      sseRoute,
	}

	return s
}

func (s *Stream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.HandleGetSSE(w, r)
	} else if r.Method == "POST" {
		s.HandlePostMessages(w, r)
	} else {
		http.NotFound(w, r)
	}
}

func (s *Stream) HandleGetSSE(w http.ResponseWriter, r *http.Request) {
	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create a flusher to ensure data is sent immediately
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	id := uuid.New().String()
	out := make(chan *mcp.Message)

	s.mu.Lock()
	s.sessions[id] = &session{
		out: out,
	}
	s.mu.Unlock()

	// Clean up session when handler exits
	defer func() {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		close(out)
	}()

	vals := r.URL.Query()
	vals.Add("session_id", id)

	session := s.messagesRoute + "?" + vals.Encode()

	writeEvent(w, "1", "endpoint", session)
	flusher.Flush()

	for {
		select {
		case msg, ok := <-out:
			if !ok {
				return
			}
			bs, err := json.Marshal(msg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeEvent(w, msg.ID.String(), "message", string(bs))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Stream) HandlePostMessages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("session_id") == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var msg mcp.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	metadata := make(map[string]string)
	for key, values := range r.URL.Query() {
		// Always pick the last value
		for _, value := range values {
			metadata[key] = value
		}
	}

	msg.Metadata = metadata

	// Use select to handle context cancellation
	select {
	case s.in <- &msg:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
		// Request was cancelled, no need to send response
		return
	}
}

func (s *Stream) Recv() (*mcp.Message, error) {
	return <-s.in, nil
}

func (s *Stream) Send(msg *mcp.Message) error {
	if msg.Metadata == nil {
		return fmt.Errorf("metadata is nil")
	}
	sessionID := msg.Metadata["session_id"]
	if sessionID == "" {
		return fmt.Errorf("session id is empty")
	}
	s.mu.RLock()
	session, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found")
	}
	go func() {
		session.out <- msg
	}()
	return nil
}
