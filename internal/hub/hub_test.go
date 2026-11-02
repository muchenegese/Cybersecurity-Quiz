package hub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBroadcastReachesEveryRoleWithItsOwnPayload(t *testing.T) {
	h := New()
	host := h.Subscribe("100200", RoleHost, "")
	p1 := h.Subscribe("100200", RoleParticipant, "p1")
	p2 := h.Subscribe("100200", RoleParticipant, "p2")
	other := h.Subscribe("999999", RoleParticipant, "px")

	h.Broadcast("100200", func(s *Sub) Frame {
		if s.Role == RoleHost {
			return Frame("host-frame")
		}
		return Frame("frame-for-" + s.ParticipantID)
	})

	if got := recv(t, host); got != "host-frame" {
		t.Errorf("host got %q", got)
	}
	if got := recv(t, p1); got != "frame-for-p1" {
		t.Errorf("p1 got %q", got)
	}
	if got := recv(t, p2); got != "frame-for-p2" {
		t.Errorf("p2 got %q", got)
	}
	select {
	case f := <-other.C():
		t.Errorf("a subscriber in another room received %q", string(f))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBuilderCanSkipASubscriber(t *testing.T) {
	h := New()
	host := h.Subscribe("1", RoleHost, "")
	part := h.Subscribe("1", RoleParticipant, "p1")

	h.Broadcast("1", func(s *Sub) Frame {
		if s.Role == RoleParticipant {
			return nil // host-only update
		}
		return Frame("x")
	})
	if got := recv(t, host); got != "x" {
		t.Errorf("host got %q", got)
	}
	select {
	case f := <-part.C():
		t.Errorf("participant should have been skipped, got %q", string(f))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSlowSubscriberDoesNotBlockTheRoom(t *testing.T) {
	h := New()
	slow := h.Subscribe("1", RoleParticipant, "slow")
	fast := h.Subscribe("1", RoleParticipant, "fast")

	// `fast` keeps draining; `slow` never reads at all. Once slow's buffer fills
	// its frames are dropped, but the broadcast must still complete for everyone
	// else — one wedged phone cannot stall a room of 500.
	var delivered int64
	go func() {
		for range fast.C() {
			atomic.AddInt64(&delivered, 1)
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			h.Broadcast("1", func(*Sub) Frame { return Frame("f") })
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast stalled behind a subscriber that stopped reading")
	}

	// The healthy subscriber should have received far more than the buffer size,
	// proving frames kept flowing rather than everyone jamming behind `slow`.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt64(&delivered); n < 1000 {
		t.Errorf("healthy subscriber received only %d frames", n)
	}
	if len(slow.C()) != cap(slow.C()) {
		t.Errorf("stalled subscriber buffer = %d, want it capped at %d", len(slow.C()), cap(slow.C()))
	}
}

func TestUnsubscribeIsIdempotentAndClosesTheChannel(t *testing.T) {
	h := New()
	s := h.Subscribe("1", RoleParticipant, "p1")
	h.Unsubscribe("1", s)
	h.Unsubscribe("1", s) // must not panic when called twice

	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Error("Done should be closed after unsubscribe")
	}
	if h.Size("1") != 0 {
		t.Error("empty room should be reclaimed")
	}
	// Sending to a closed subscriber must be a no-op, not a panic.
	h.Broadcast("1", func(*Sub) Frame { return Frame("x") })
}

func TestPresenceCountsByRole(t *testing.T) {
	h := New()
	h.Subscribe("1", RoleHost, "")
	h.Subscribe("1", RoleScreen, "")
	h.Subscribe("1", RoleParticipant, "p1")
	h.Subscribe("1", RoleParticipant, "p2")
	p2b := h.Subscribe("1", RoleParticipant, "p2") // same person, second tab

	hosts, screens, people := h.Presence("1")
	if hosts != 1 || screens != 1 {
		t.Errorf("hosts=%d screens=%d, want 1/1", hosts, screens)
	}
	if len(people) != 2 {
		t.Errorf("distinct participants = %d, want 2 (a second tab is the same person)", len(people))
	}
	h.Unsubscribe("1", p2b)
	if _, _, people = h.Presence("1"); len(people) != 2 {
		t.Error("closing one of two tabs should leave the participant present")
	}
}

func TestConcurrentSubscribeAndBroadcast(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Broadcast("room", func(*Sub) Frame { return Frame("tick") })
			}
		}
	}()

	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := h.Subscribe("room", RoleParticipant, "p")
			go func() {
				for range s.C() {
				}
			}()
			time.Sleep(5 * time.Millisecond)
			h.Unsubscribe("room", s)
		}()
	}
	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func recv(t *testing.T, s *Sub) string {
	t.Helper()
	select {
	case f := <-s.C():
		return string(f)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a frame")
		return ""
	}
}
