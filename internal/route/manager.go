package route

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antonismor/Titanus-Core/internal/realm"
)

type StateProvider func() (realm.State, error)

type Manager struct {
	state StateProvider

	mu        sync.Mutex
	listeners map[string]*routeListener
}

type routeListener struct {
	spec     realm.Route
	listener net.Listener
	cancel   context.CancelFunc
	rr       atomic.Uint64
}

func NewManager(store *realm.Store) *Manager {
	if store == nil {
		return NewManagerWithStateProvider(nil)
	}
	return NewManagerWithStateProvider(func() (realm.State, error) {
		return store.Snapshot(), nil
	})
}

func NewManagerWithStateProvider(provider StateProvider) *Manager {
	return &Manager{state: provider, listeners: map[string]*routeListener{}}
}

func (m *Manager) snapshot() (realm.State, error) {
	if m.state == nil {
		return realm.State{}, fmt.Errorf("Route manager has no Realm state provider")
	}
	return m.state()
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_ = m.Reconcile()
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) Reconcile() error {
	state, err := m.snapshot()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, running := range m.listeners {
		desired, ok := state.Routes[name]
		if !ok || !sameListener(running.spec, desired) {
			running.cancel()
			_ = running.listener.Close()
			delete(m.listeners, name)
		}
	}
	for name, desired := range state.Routes {
		if _, ok := m.listeners[name]; ok {
			continue
		}
		address := net.JoinHostPort(desired.ListenIP, strconv.Itoa(desired.ListenPort))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("Route %s listen %s: %w", name, address, err)
		}
		routeCtx, cancel := context.WithCancel(context.Background())
		running := &routeListener{spec: desired, listener: listener, cancel: cancel}
		m.listeners[name] = running
		go m.serve(routeCtx, running)
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, running := range m.listeners {
		running.cancel()
		_ = running.listener.Close()
		delete(m.listeners, name)
	}
}

func (m *Manager) serve(ctx context.Context, running *routeListener) {
	for {
		conn, err := running.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go m.proxy(ctx, running, conn)
	}
}

func (m *Manager) proxy(ctx context.Context, running *routeListener, client net.Conn) {
	defer client.Close()

	backends := m.backends(running.spec)
	if len(backends) == 0 {
		return
	}
	start := int(running.rr.Add(1)-1) % len(backends)

	var upstream net.Conn
	for i := 0; i < len(backends); i++ {
		index := (start + i) % len(backends)
		dialer := net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", backends[index])
		if err == nil {
			upstream = conn
			break
		}
	}
	if upstream == nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (m *Manager) backends(route realm.Route) []string {
	state, err := m.snapshot()
	if err != nil {
		return nil
	}
	out := make([]string, 0)
	for _, assignment := range state.Assignments {
		if assignment.Fleet != route.Fleet ||
			assignment.State != realm.AssignmentActive ||
			assignment.NetworkAddress == "" {
			continue
		}
		node, ok := state.Nodes[assignment.NodeID]
		if !ok || node.State != realm.NodeReady {
			continue
		}
		out = append(out, net.JoinHostPort(assignment.NetworkAddress, strconv.Itoa(route.TargetPort)))
	}
	sort.Strings(out)
	return out
}

func sameListener(a, b realm.Route) bool {
	return a.Name == b.Name &&
		a.ListenIP == b.ListenIP &&
		a.ListenPort == b.ListenPort &&
		a.TargetPort == b.TargetPort &&
		a.Protocol == b.Protocol &&
		a.Fleet == b.Fleet
}
