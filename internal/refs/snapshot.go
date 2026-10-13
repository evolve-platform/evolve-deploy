package refs

import (
	"context"
	"fmt"
	"sync"
)

// Pinner resolves version selectors using metadata only. Native references stay
// references; a ParameterVersion becomes the literal version ID the app reads.
type Pinner interface {
	Pin(context.Context, Value) (Value, error)
}

// Snapshot captures each selector once per plan, including across sibling
// targets. A new plan gets a new snapshot so later publications are visible.
type Snapshot struct {
	Resolver
	mu   sync.Mutex
	pins map[Value]func() (Value, error)
}

func NewSnapshot(r Resolver) *Snapshot {
	return &Snapshot{Resolver: r, pins: map[Value]func() (Value, error){}}
}

func (s *Snapshot) Pin(ctx context.Context, v Value) (Value, error) {
	s.mu.Lock()
	pin, ok := s.pins[v]
	if !ok {
		pin = sync.OnceValues(func() (Value, error) {
			if p, ok := s.Resolver.(Pinner); ok {
				return p.Pin(ctx, v)
			}
			if v.Kind == ParameterVersion {
				return Value{}, fmt.Errorf("parameter-version references are not supported by this cloud")
			}
			return v, s.Verify(ctx, v)
		})
		s.pins[v] = pin
	}
	s.mu.Unlock()
	return pin()
}
