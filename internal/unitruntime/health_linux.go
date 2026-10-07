package unitruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

func (m *Manager) RunHealth(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		states, err := m.List()
		if err != nil {
			log.Printf("runtime health scan: %v", err)
		} else {
			jobs := make(chan string)
			var workers sync.WaitGroup
			for i := 0; i < 8; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for id := range jobs {
						if ctx.Err() != nil {
							continue
						}
						if err := m.CheckHealth(ctx, id); err != nil {
							log.Printf("runtime health Unit=%s: %v", id, err)
						}
					}
				}()
			}
			for _, state := range states {
				select {
				case jobs <- state.ID:
				case <-ctx.Done():
				}
			}
			close(jobs)
			workers.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) CheckHealth(ctx context.Context, id string) error {
	spec, state, err := m.Inspect(id)
	if err != nil {
		return err
	}
	spec.Normalize()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if state.PID <= 0 {
		return m.checkRestart(id, spec, state)
	}
	if !state.DesiredRunning {
		return nil
	}
	now := time.Now().UTC()
	var readyErr, liveErr error
	checkReady := probeDue(spec.Health.Readiness, state.Readiness, state.StartedAt, now)
	checkLive := probeDue(spec.Health.Liveness, state.Liveness, state.StartedAt, now)
	if checkReady {
		readyErr = m.runProbe(ctx, state, *spec.Health.Readiness)
	}
	if checkLive {
		liveErr = m.runProbe(ctx, state, *spec.Health.Liveness)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !checkReady && !checkLive {
		return nil
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	_, current, err := m.load(id)
	if err != nil {
		unlock()
		return err
	}
	if current.RunID != state.RunID || !processMatches(current) {
		unlock()
		return nil
	}
	if checkReady {
		current.Readiness = advanceProbe(current.Readiness, readyErr, now)
		if current.Readiness.Successes >= spec.Health.Readiness.SuccessThreshold {
			current.Ready = true
		}
		if current.Readiness.Failures >= spec.Health.Readiness.FailureThreshold {
			current.Ready = false
		}
	}
	stop := false
	if checkLive {
		current.Liveness = advanceProbe(current.Liveness, liveErr, now)
		if current.Liveness.Successes >= spec.Health.Liveness.SuccessThreshold {
			current.Live = true
		}
		if current.Liveness.Failures >= spec.Health.Liveness.FailureThreshold {
			current.Live = false
			current.Ready = false
			stop = true
		}
	}
	if time.Since(current.StartedAt) > 10*time.Minute {
		current.RestartCount = 0
	}
	err = m.saveState(current)
	unlock()
	if err != nil {
		return err
	}
	if stop {
		_, err = m.stop(id, 5*time.Second, true, current.RunID)
	}
	return err
}

func probeDue(probe *Probe, result ProbeResult, started, now time.Time) bool {
	return probe != nil && !now.Before(started.Add(time.Duration(probe.InitialDelaySeconds)*time.Second)) && (result.CheckedAt.IsZero() || !now.Before(result.CheckedAt.Add(time.Duration(probe.IntervalSeconds)*time.Second)))
}

func (m *Manager) checkRestart(id string, spec Spec, state State) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	_, current, err := m.load(id)
	if err != nil {
		unlock()
		return err
	}
	if current.RunID != state.RunID || current.PID > 0 {
		unlock()
		return nil
	}
	due := restartDue(spec, &current, time.Now().UTC())
	err = m.saveState(current)
	unlock()
	if err != nil || !due {
		return err
	}
	_, err = m.start(id, true)
	return err
}

// EnsureRunning is the controller's recurring reconciliation operation. Unlike
// an explicit operator Start, it honors persisted backoff and restart limits.
func (m *Manager) EnsureRunning(id string) (State, error) {
	spec, state, err := m.Inspect(id)
	if err != nil {
		return State{}, err
	}
	spec.Normalize()
	if state.Status == StatusCreated || !state.DesiredRunning && state.Status == StatusStopped {
		return m.Start(id)
	}
	if err := m.checkRestart(id, spec, state); err != nil && !errors.Is(err, context.Canceled) {
		return state, fmt.Errorf("reconcile restart: %w", err)
	}
	_, state, err = m.Inspect(id)
	return state, err
}
