package roadrunner

import (
	"context"
	"os/exec"

	"github.com/spiral/errors"
	"github.com/spiral/roadrunner/v2/util"
)

// StopRequest can be sent by worker to indicate that restart is required.
const StopRequest = "{\"stop\":true}"

var bCtx = context.Background()

// Allocator is responsible for worker allocation in the pool
type Allocator func() (WorkerBase, error)

// ErrorEncoder encode error or make a decision based on the error type
type ErrorEncoder func(err error, w WorkerBase) (Payload, error)

// PoolBefore is set of functions that executes BEFORE Exec
type Before func(req Payload) Payload

// PoolAfter is set of functions that executes AFTER Exec
type After func(req Payload, resp Payload) Payload

type PoolOptions func(p *StaticPool)

// StaticPool controls worker creation, destruction and task routing. Pool uses fixed amount of stack.
type StaticPool struct {
	cfg Config

	// worker command creator
	cmd func() *exec.Cmd

	// creates and connects to stack
	factory Factory

	// distributes the events
	events util.EventsHandler

	// manages worker states and TTLs
	ww WorkerWatcher

	// allocate new worker
	allocator Allocator

	errEncoder ErrorEncoder
	before     []Before
	after      []After
}

// NewPool creates new worker pool and task multiplexer. StaticPool will initiate with one worker.
func NewPool(ctx context.Context, cmd func() *exec.Cmd, factory Factory, cfg Config, options ...PoolOptions) (Pool, error) {
	const op = errors.Op("NewPool")
	cfg.InitDefaults()

	if cfg.Debug {
		cfg.NumWorkers = 0
		cfg.MaxJobs = 1
	}

	p := &StaticPool{
		cfg:     cfg,
		cmd:     cmd,
		factory: factory,
		events:  util.NewEventsHandler(),
		after:   make([]After, 0, 0),
		before:  make([]Before, 0, 0),
	}

	var err error
	p.allocator, err = newPoolAllocator(factory, cmd)
	if err != nil {
		return nil, errors.E(op, err)
	}

	p.ww = newWorkerWatcher(p.allocator, p.cfg.NumWorkers, p.events)

	workers, err := p.allocateWorkers(ctx, p.cfg.NumWorkers)
	if err != nil {
		return nil, err
	}

	// put stack in the pool
	err = p.ww.AddToWatch(ctx, workers)
	if err != nil {
		return nil, err
	}

	p.errEncoder = defaultErrEncoder(p)

	// add pool options
	for i := 0; i < len(options); i++ {
		options[i](p)
	}

	// if supervised config not nil, guess, that pool wanted to be supervised
	if cfg.Supervisor != nil {
		sp := newPoolWatcher(p, p.events, p.cfg.Supervisor)
		// start watcher timer
		sp.Start()
		return sp, nil
	}

	return p, nil
}

func PoolBefore(before ...Before) PoolOptions {
	return func(p *StaticPool) {
		p.before = append(p.before, before...)
	}
}

func PoolAfter(after ...After) PoolOptions {
	return func(p *StaticPool) {
		p.after = append(p.after, after...)
	}
}

// AddListener connects event listener to the pool.
func (sp *StaticPool) AddListener(listener util.EventListener) {
	sp.events.AddListener(listener)
}

// Config returns associated pool configuration. Immutable.
func (sp *StaticPool) GetConfig() Config {
	return sp.cfg
}

// Workers returns worker list associated with the pool.
func (sp *StaticPool) Workers() (workers []WorkerBase) {
	return sp.ww.WorkersList()
}

func (sp *StaticPool) RemoveWorker(ctx context.Context, wb WorkerBase) error {
	return sp.ww.RemoveWorker(ctx, wb)
}

func (sp *StaticPool) Exec(p Payload) (Payload, error) {
	const op = errors.Op("Exec")
	if sp.cfg.Debug {
		return sp.execDebug(p)
	}
	w, err := sp.ww.GetFreeWorker(context.Background())
	if err != nil {
		return EmptyPayload, errors.E(op, err)
	}

	sw := w.(SyncWorker)

	if len(sp.before) > 0 {
		for i := 0; i < len(sp.before); i++ {
			p = sp.before[i](p)
		}
	}

	rsp, err := sw.Exec(p)
	if err != nil {
		return sp.errEncoder(err, sw)
	}

	// worker want's to be terminated
	if rsp.Body == nil && rsp.Context != nil && string(rsp.Context) == StopRequest {
		sw.State().Set(StateInvalid)
		err = sw.Stop(bCtx)
		if err != nil {
			sp.events.Push(WorkerEvent{Event: EventWorkerError, Worker: sw, Payload: errors.E(op, err)})
		}

		return sp.Exec(p)
	}

	if sp.cfg.MaxJobs != 0 && sw.State().NumExecs() >= sp.cfg.MaxJobs {
		err = sp.ww.AllocateNew(bCtx)
		if err != nil {
			return EmptyPayload, errors.E(op, err)
		}
	} else {
		sp.ww.PushWorker(sw)
	}

	if len(sp.after) > 0 {
		for i := 0; i < len(sp.after); i++ {
			rsp = sp.after[i](p, rsp)
		}
	}

	return rsp, nil
}

func (sp *StaticPool) ExecWithContext(ctx context.Context, rqs Payload) (Payload, error) {
	const op = errors.Op("Exec with context")
	w, err := sp.ww.GetFreeWorker(context.Background())
	if err != nil {
		return EmptyPayload, errors.E(op, err)
	}

	sw := w.(SyncWorker)

	if len(sp.before) > 0 {
		for i := 0; i < len(sp.before); i++ {
			rqs = sp.before[i](rqs)
		}
	}

	rsp, err := sw.ExecWithContext(ctx, rqs)
	if err != nil {
		return sp.errEncoder(err, sw)
	}

	// worker want's to be terminated
	if rsp.Body == nil && rsp.Context != nil && string(rsp.Context) == StopRequest {
		sw.State().Set(StateInvalid)
		err = sw.Stop(bCtx)
		if err != nil {
			sp.events.Push(WorkerEvent{Event: EventWorkerError, Worker: sw, Payload: errors.E(op, err)})
		}

		return sp.Exec(rqs)
	}

	if sp.cfg.MaxJobs != 0 && sw.State().NumExecs() >= sp.cfg.MaxJobs {
		err = sp.ww.AllocateNew(bCtx)
		if err != nil {
			return EmptyPayload, errors.E(op, err)
		}
	} else {
		sp.ww.PushWorker(sw)
	}

	if len(sp.after) > 0 {
		for i := 0; i < len(sp.after); i++ {
			rsp = sp.after[i](rqs, rsp)
		}
	}

	return rsp, nil
}

// Destroy all underlying stack (but let them to complete the task).
func (sp *StaticPool) Destroy(ctx context.Context) {
	sp.ww.Destroy(ctx)
}

func defaultErrEncoder(sp *StaticPool) ErrorEncoder {
	return func(err error, w WorkerBase) (Payload, error) {
		const op = errors.Op("error encoder")
		// soft job errors are allowed
		if errors.Is(errors.Exec, err) {
			if sp.cfg.MaxJobs != 0 && w.State().NumExecs() >= sp.cfg.MaxJobs {
				err = sp.ww.AllocateNew(bCtx)
				if err != nil {
					sp.events.Push(PoolEvent{Event: EventPoolError, Payload: errors.E(op, err)})
				}

				w.State().Set(StateInvalid)
				err = w.Stop(bCtx)
				if err != nil {
					sp.events.Push(WorkerEvent{Event: EventWorkerError, Worker: w, Payload: errors.E(op, err)})
				}
			} else {
				sp.ww.PushWorker(w)
			}

			return EmptyPayload, errors.E(op, err)
		}

		w.State().Set(StateInvalid)
		sp.events.Push(PoolEvent{Event: EventWorkerDestruct, Payload: w})
		errS := w.Stop(bCtx)

		if errS != nil {
			return EmptyPayload, errors.E(op, errors.Errorf("%v, %v", err, errS))
		}

		return EmptyPayload, errors.E(op, err)
	}
}

func newPoolAllocator(factory Factory, cmd func() *exec.Cmd) (Allocator, error) {
	return func() (WorkerBase, error) {
		w, err := factory.SpawnWorkerWithContext(bCtx, cmd())
		if err != nil {
			return nil, err
		}

		sw, err := NewSyncWorker(w)
		if err != nil {
			return nil, err
		}
		return sw, nil
	}, nil
}

func (sp *StaticPool) execDebug(p Payload) (Payload, error) {
	sw, err := sp.allocator()
	if err != nil {
		return EmptyPayload, err
	}

	r, err := sw.(SyncWorker).Exec(p)

	if stopErr := sw.Stop(context.Background()); stopErr != nil {
		sp.events.Push(WorkerEvent{Event: EventWorkerError, Worker: sw, Payload: err})
	}

	return r, err
}

// allocate required number of stack
func (sp *StaticPool) allocateWorkers(ctx context.Context, numWorkers int64) ([]WorkerBase, error) {
	const op = errors.Op("allocate workers")
	var workers []WorkerBase

	// constant number of stack simplify logic
	for i := int64(0); i < numWorkers; i++ {
		ctx, cancel := context.WithTimeout(ctx, sp.cfg.AllocateTimeout)
		w, err := sp.factory.SpawnWorkerWithContext(ctx, sp.cmd())
		if err != nil {
			cancel()
			return nil, errors.E(op, err)
		}
		workers = append(workers, w)
		cancel()
	}
	return workers, nil
}

func (sp *StaticPool) checkMaxJobs(ctx context.Context, w WorkerBase) error {
	const op = errors.Op("check max jobs")
	if sp.cfg.MaxJobs != 0 && w.State().NumExecs() >= sp.cfg.MaxJobs {
		err := sp.ww.AllocateNew(ctx)
		if err != nil {
			sp.events.Push(PoolEvent{Event: EventPoolError, Payload: err})
			return errors.E(op, err)
		}
	}
	return nil
}
