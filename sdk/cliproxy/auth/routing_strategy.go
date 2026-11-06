package auth

import (
	"context"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func cloneSmoothState(state *smoothWeightedState) smoothWeightedState {
	if state == nil {
		return smoothWeightedState{}
	}
	return smoothWeightedState{current: maps.Clone(state.current), weights: maps.Clone(state.weights)}
}

func (c *routingCapture) candidateIndex(a *Auth) int {
	if i, ok := c.indexes[a.ID]; ok {
		return i
	}
	i := len(c.trace.Candidates)
	clone := a.Clone()
	c.trace.Candidates = append(c.trace.Candidates, routing.Candidate{ID: a.ID, Provider: a.Provider, Priority: authPriority(a), AuthIndex: clone.EnsureIndex()})
	c.indexes[a.ID] = i
	return i
}

// strategySimulation owns copies of every cursor and accumulator it advances.
type strategySimulation struct {
	pool    []*Auth
	state   smoothWeightedState
	weights map[string]int64
	last    string
	next    func() *Auth
}

func strategyStep(c *routingCapture, strategy string, pool []*Auth, model string, halfLife time.Duration, last string, state *smoothWeightedState, prepared ...map[string]routing.StrategyWeight) (*routing.StrategyStep, *strategySimulation) {
	step := &routing.StrategyStep{Applied: strategy, Last: -1, Wanted: -1, Weights: make([]routing.StrategyWeight, 0, len(pool))}
	sim := &strategySimulation{pool: pool, state: cloneSmoothState(state), last: last}
	if i, ok := c.indexes[last]; last != "" && ok {
		step.Last = i
	}
	var factors map[string]routing.StrategyWeight
	if strategy == routing.StrategyGreedySoonestRecovery {
		if halfLife <= 0 {
			halfLife = defaultGreedySoonestRecoveryHalfLife
		}
		step.HalfLifeMs = halfLife.Milliseconds()
		var err error
		if len(prepared) > 0 {
			factors = prepared[0]
			if factors == nil {
				err = greedySoonestRecoveryTimestampError("mixed", model)
			}
		} else {
			factors, err = greedySoonestRecoveryFactors(pool, "mixed", model, time.Now(), halfLife)
		}
		if err != nil {
			step.Applied = routing.StrategyRoundRobin
			step.Fallback = "missing_recovery_data"
		}
	}
	if step.Applied == routing.StrategyWeightedRoundRobin {
		sim.weights = authWeightVector(pool)
	}
	if step.Applied == routing.StrategyGreedySoonestRecovery {
		sim.weights = make(map[string]int64, len(pool))
		for id, f := range factors {
			sim.weights[id] = f.Weight
		}
	}
	if sim.weights != nil {
		sim.state.prepare(sim.weights)
	}
	var total float64
	for _, w := range sim.weights {
		total += float64(w)
	}
	for i, a := range pool {
		w := factors[a.ID]
		w.Index = c.candidateIndex(a)
		w.ConfigWeight = authWeight(a)
		if sim.weights != nil {
			w.Credit = sim.state.current[a.ID]
		}
		switch step.Applied {
		case routing.StrategyFillFirst:
			if i == 0 {
				w.Share = 1
			}
		case routing.StrategyRoundRobin:
			w.Share = 1 / float64(len(pool))
		default:
			w.Weight = sim.weights[a.ID]
			if total > 0 {
				w.Share = float64(w.Weight) / total
			}
		}
		step.Weights = append(step.Weights, w)
	}
	sim.next = func() *Auth {
		if len(pool) == 0 {
			return nil
		}
		if sim.weights != nil {
			return pickSmoothWeightedAuthWithWeights(pool, sim.weights, sim.state.current)
		}
		if step.Applied == routing.StrategyFillFirst {
			return pool[0]
		}
		a := pool[successorIndex(pool, sim.last)]
		sim.last = a.ID
		return a
	}
	return step, sim
}

func captureStrategyPick(ctx context.Context, strategy string, pool []*Auth, model string, halfLife time.Duration, last string, state *smoothWeightedState, picked *Auth, prepared ...map[string]routing.StrategyWeight) {
	if ctx == nil {
		return
	}
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	step, sim := strategyStep(c, strategy, pool, model, halfLife, last, state, prepared...)
	if picked == nil {
		picked = sim.next()
	}
	if picked != nil {
		step.Wanted = c.candidateIndex(picked)
	}
	c.trace.StrategyStep = step
}

func markStrategyFallback(ctx context.Context, halfLife time.Duration) {
	if ctx == nil {
		return
	}
	if c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture); c != nil && c.trace.StrategyStep != nil {
		c.trace.StrategyStep.Fallback = "missing_recovery_data"
		c.trace.StrategyStep.HalfLifeMs = halfLife.Milliseconds()
	}
}

// scheduledStrategySimulation observes existing ready views under the scheduler lock.
// It never creates a shard, promotes entries, or changes live picker state.
func (s *authScheduler) scheduledStrategySimulation(c *routingCapture, shards []*modelScheduler, preferWS bool, priority int, predicate func(*scheduledAuth) bool, strategy schedulerStrategy, providers []string, model string, prepared ...map[string]routing.StrategyWeight) (*routing.StrategyStep, *strategySimulation) {
	var pool []*Auth
	views := make(map[string]*readyView)
	var state *smoothWeightedState
	last := ""
	for i, shard := range shards {
		if shard == nil {
			continue
		}
		bucket := shard.readyByPriority[priority]
		if bucket == nil {
			continue
		}
		view := &bucket.all
		if preferWS && bucket.ws.pickFirst(predicate) != nil {
			view = &bucket.ws
		}
		views[providers[i]] = view
		for _, e := range view.flat {
			if e != nil && e.auth != nil && (predicate == nil || predicate(e)) {
				pool = append(pool, e.auth)
			}
		}
		if len(shards) == 1 {
			last = view.lastPicked
			state = &view.weightedState
			if strategy == schedulerStrategyGreedySoonestRecovery {
				state = &view.gsrState
			}
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })
	name := routing.StrategyRoundRobin
	switch strategy {
	case schedulerStrategyFillFirst:
		name = routing.StrategyFillFirst
	case schedulerStrategyWeightedRoundRobin:
		name = routing.StrategyWeightedRoundRobin
	case schedulerStrategyGreedySoonestRecovery:
		name = routing.StrategyGreedySoonestRecovery
	}
	cursorKey := strings.Join(providers, ",") + ":" + canonicalModelKey(model)
	if len(shards) > 1 {
		state = s.mixedWeightedStates[cursorKey]
	}
	step, sim := strategyStep(c, name, pool, model, defaultGreedySoonestRecoveryHalfLife, last, state, prepared...)
	if len(shards) > 1 && len(pool) > 0 {
		slot := s.mixedCursors[cursorKey]
		lasts := make(map[string]string)
		groups := make(map[string][]*Auth)
		for _, p := range providers {
			if v := views[p]; v != nil {
				lasts[p] = v.lastPicked
			}
		}
		for _, a := range pool {
			p := canonicalSchedulingProvider(executorKeyFromAuth(a))
			groups[p] = append(groups[p], a)
		}
		if strategy == schedulerStrategyGreedySoonestRecovery && step.Fallback != "" {
			sim.next = func() *Auth { a := pool[slot%len(pool)]; slot = (slot + 1) % len(pool); return a }
			if _, seen := s.mixedCursors[cursorKey]; seen {
				step.Last = c.candidateIndex(pool[(slot-1+len(pool))%len(pool)])
			}
		} else if strategy == schedulerStrategyRoundRobin || strategy == schedulerStrategyFillFirst {
			if strategy == schedulerStrategyFillFirst {
				for _, p := range providers {
					if len(groups[p]) > 0 {
						first := groups[p][0]
						sim.next = func() *Auth { return first }
						for i := range step.Weights {
							step.Weights[i].Share = 0
							if step.Weights[i].Index == c.candidateIndex(first) {
								step.Weights[i].Share = 1
							}
						}
						break
					}
				}
			} else {
				if _, seen := s.mixedCursors[cursorKey]; seen {
					previousSlot := (slot - 1 + len(pool)) % len(pool)
					for _, p := range providers {
						if previousSlot >= len(groups[p]) {
							previousSlot -= len(groups[p])
							continue
						}
						if i, ok := c.indexes[lasts[p]]; ok && lasts[p] != "" {
							step.Last = i
						}
						break
					}
				}
				sim.next = func() *Auth {
					offset := slot % len(pool)
					for _, p := range providers {
						g := groups[p]
						if offset >= len(g) {
							offset -= len(g)
							continue
						}
						a := g[successorIndex(g, lasts[p])]
						lasts[p] = a.ID
						slot++
						return a
					}
					return nil
				}
			}
		}
	}
	return step, sim
}

func (s *authScheduler) captureScheduledStrategy(ctx context.Context, shards []*modelScheduler, preferWS bool, priority int, predicate func(*scheduledAuth) bool, strategy schedulerStrategy, providers []string, model string, prepared ...map[string]routing.StrategyWeight) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	step, sim := s.scheduledStrategySimulation(c, shards, preferWS, priority, predicate, strategy, providers, model, prepared...)
	if a := sim.next(); a != nil {
		step.Wanted = c.candidateIndex(a)
	}
	c.trace.StrategyStep = step
}

func (s *authScheduler) captureSingleStrategy(ctx context.Context, shard *modelScheduler, preferWS bool, predicate func(*scheduledAuth) bool, strategy schedulerStrategy, provider, model string) {
	if c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture); c == nil || shard == nil {
		return
	}
	if priority, ok := shard.highestReadyPriorityLocked(preferWS, predicate); ok {
		s.captureScheduledStrategy(ctx, []*modelScheduler{shard}, preferWS, priority, predicate, strategy, []string{provider}, model)
	}
}
