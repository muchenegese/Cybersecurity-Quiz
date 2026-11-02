// Package hub implements the realtime fan-out layer.
//
// KwiqTest uses Server-Sent Events rather than WebSockets. The traffic pattern
// is almost entirely one-way — the server pushes question/timer/leaderboard
// state to every client, and clients send their rare actions as ordinary POSTs
// — so SSE gives the same sub-second sync with far less machinery: no upgrade
// handshake, no ping/pong keepalive protocol to hand-roll, no extra dependency,
// and browsers reconnect automatically after a network drop, which is exactly
// the rejoin behaviour the PRD asks for.
package hub

import (
	"sync"
	"sync/atomic"
)

// Role identifies what a subscriber is allowed to see.
type Role string

const (
	RoleHost        Role = "host"
	RoleParticipant Role = "participant"
	RoleScreen      Role = "screen" // read-only projector view
)

// Sub is one connected client.
type Sub struct {
	ID            uint64
	Role          Role
	ParticipantID string
	ch            chan []byte
	done          chan struct{}
	closeOnce     sync.Once
}

// C is the frame channel the handler reads from. It is deliberately never
// closed: closing a channel concurrently with a send is a data race, and
// broadcasts run concurrently with clients disconnecting all the time. Readers
// select on Done instead.
func (s *Sub) C() <-chan []byte { return s.ch }

// Done is closed once the subscriber has been removed.
func (s *Sub) Done() <-chan struct{} { return s.done }

func (s *Sub) shutdown() { s.closeOnce.Do(func() { close(s.done) }) }

// Frame is a pre-encoded SSE payload, built once and shared by every
// subscriber that should receive it.
type Frame []byte

// Builder returns the frame a given subscriber should receive, or nil to skip.
type Builder func(s *Sub) Frame

// Hub holds one room per session code.
type Hub struct {
	mu     sync.RWMutex
	rooms  map[string]map[uint64]*Sub
	nextID atomic.Uint64

	// OnChange is called whenever a room's membership changes, so the session
	// engine can react to connect/disconnect without polling.
	OnChange func(code string)
}

// New creates an empty hub.
func New() *Hub {
	return &Hub{rooms: map[string]map[uint64]*Sub{}}
}

// Subscribe registers a client in a room. The returned Sub's channel is closed
// when Unsubscribe is called.
func (h *Hub) Subscribe(code string, role Role, participantID string) *Sub {
	s := &Sub{
		ID:            h.nextID.Add(1),
		Role:          role,
		ParticipantID: participantID,
		// Buffered so a briefly stalled client (backgrounded phone tab) does
		// not block the broadcast loop for everyone else.
		ch:   make(chan []byte, 32),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	room := h.rooms[code]
	if room == nil {
		room = map[uint64]*Sub{}
		h.rooms[code] = room
	}
	room[s.ID] = s
	h.mu.Unlock()
	if h.OnChange != nil {
		h.OnChange(code)
	}
	return s
}

// Unsubscribe removes a client. It is safe to call more than once.
func (h *Hub) Unsubscribe(code string, s *Sub) {
	h.mu.Lock()
	if room := h.rooms[code]; room != nil {
		delete(room, s.ID)
		if len(room) == 0 {
			delete(h.rooms, code)
		}
	}
	h.mu.Unlock()
	s.shutdown()
	if h.OnChange != nil {
		h.OnChange(code)
	}
}

// Broadcast sends to every subscriber in a room. build is called once per
// subscriber so payloads can differ by role; callers that send the same bytes
// to everyone should close over a single pre-encoded frame.
func (h *Hub) Broadcast(code string, build Builder) {
	h.mu.RLock()
	room := h.rooms[code]
	subs := make([]*Sub, 0, len(room))
	for _, s := range room {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		frame := build(s)
		if frame == nil {
			continue
		}
		h.send(s, frame)
	}
}

// SendTo delivers a frame to one specific subscriber id.
func (h *Hub) SendTo(code string, subID uint64, frame Frame) {
	h.mu.RLock()
	s := h.rooms[code][subID]
	h.mu.RUnlock()
	if s != nil {
		h.send(s, frame)
	}
}

// send never blocks. If a client's buffer is full it has fallen too far behind
// to be useful; we drop the frame rather than stall the session. The client's
// next reconnect (or the next state change) resynchronises it from scratch,
// because every frame carries complete state rather than a delta.
func (h *Hub) send(s *Sub, frame []byte) {
	select {
	case <-s.done: // already gone
		return
	case s.ch <- frame:
	default: // buffer full: drop this frame, the next one carries full state
	}
}

// Presence reports how many clients of each kind are in a room, plus the set of
// participant ids currently connected.
func (h *Hub) Presence(code string) (hosts, screens int, participants map[string]bool) {
	participants = map[string]bool{}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.rooms[code] {
		switch s.Role {
		case RoleHost:
			hosts++
		case RoleScreen:
			screens++
		case RoleParticipant:
			if s.ParticipantID != "" {
				participants[s.ParticipantID] = true
			}
		}
	}
	return
}

// Size reports the number of subscribers in a room.
func (h *Hub) Size(code string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[code])
}
