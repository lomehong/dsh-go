// Package jobs ports @deepseek-ai/dsh-jobs + @deepseek-ai/dsh-jobs-local:
// the background-job registry (`ctx.jobs`). It owns job ids, session-scoped
// access, lifecycle state, completion listeners, and owner cleanup while
// producers retain their execution resources. Records stay in memory and
// every read hands out a fresh snapshot, never live state.
//
// Go adaptation: the owner is an interface carrying the session id and the
// owner's scope key (the official Agent + scopeOf(owner.ctx)); the caller is
// a session id; listener/observer/controller layers are registered with an
// explicit scope key; and owner disposal is delivered through DisposeOwner —
// the agent lifecycle calls it where the official registry attaches a
// scoped effect itself.
package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dshgo/scope"
)

// Job statuses. Exactly one terminal status follows an optional stopping.
const (
	StatusRunning   = "running"
	StatusStopping  = "stopping"
	StatusCompleted = "completed"
	StatusKilled    = "killed"
	StatusFailed    = "failed"
)

// Terminal outcome statuses a producer reports through Hooks.
const (
	OutcomeCompleted = "completed"
	OutcomeKilled    = "killed"
	OutcomeFailed    = "failed"
)

// TaskWaitTimeout is the code that distinguishes a bounded wait from caller
// cancellation.
const TaskWaitTimeout = "TASK_WAIT_TIMEOUT"

// DefaultMaxConcurrentJobsPerOwner is the default active-job limit for one
// exact-owner bucket (or the shared unowned bucket).
const DefaultMaxConcurrentJobsPerOwner = 10

// Outcome is the terminal result a producer supplies.
type Outcome struct {
	// Status: finished (completed), cancelled (killed), or broke (failed).
	Status string
	// Detail is kind-specific text rendered into status lines.
	Detail string
	// Output is the final output for jobs without ReadOutput; stream jobs
	// leave it unset.
	Output string
}

// Hooks carries the runtime's control and observation points into producer
// work.
type Hooks struct {
	// Cancel requests termination: synchronous, idempotent, eventually
	// settling Done. A non-nil error propagates without changing job
	// state.
	Cancel func(reason string) error
	// Done resolves after the producer releases its resources, not merely
	// when work finishes. A result carrying Err is the producer-contract
	// violation path and settles the job failed.
	Done <-chan Result
	// ReadOutput consumes output produced since the previous call. Nil
	// marks a final-output-only job; each job has one consuming cursor.
	ReadOutput func() string
}

// Result is one observation of a producer's Done channel.
type Result struct {
	Outcome Outcome
	// Err marks a producer contract violation (the official rejected
	// done promise); the job settles failed with the error text.
	Err error
}

// StartSpec is the producer declaration passed to Start. The runtime
// preflights access and cleanup before invoking Run; the producer owns
// execution resources while the runtime owns identity and lifecycle state.
type StartSpec struct {
	// Kind is the producer kind — also the id prefix.
	Kind string
	// Label is the one-line model-facing label.
	Label string
	// OutputLimitBytes caps each complete model-facing completion notice
	// or output read; zero leaves it unset.
	OutputLimitBytes int
	// Owner is the owning live agent; nil creates an unowned job, open to
	// any caller until service disposal.
	Owner Owner
	// Run starts the work after preflight and synchronously returns its
	// hooks. Called once; an error leaves nothing registered, and the
	// producer must clean up any partially started resources.
	Run func() (Hooks, error)
}

// Owner is the agent lifecycle that owns a job: access is fenced by the id,
// layer resolution walks the scope, and disposal cancels the work.
type Owner interface {
	OwnerID() string
	OwnerScope() scope.ScopeKey
}

// Snapshot is a read-only projection of one job — a fresh object per call,
// never live registry state.
type Snapshot struct {
	// ID is the registry-issued `<kind>-N` id.
	ID string
	// Kind is the producer kind the job was registered with.
	Kind string
	// Label is the producer-supplied one-line label.
	Label string
	// OutputLimitBytes is the producer-owned cap; zero leaves it unset.
	OutputLimitBytes int
	// OwnerSession is the owner session id used for authorization; empty
	// for unowned jobs.
	OwnerSession string
	// Status is the current lifecycle state.
	Status string
	// Detail is kind-specific status detail, present once the producer
	// supplied one (usually terminal).
	Detail string
	// StartedAt is the epoch ms when the job was registered.
	StartedAt int64
	// FinishedAt is the epoch ms when the job settled; zero while
	// running/stopping.
	FinishedAt int64
	// Reported is true when a kill, read, wait, or teardown cancel has
	// reported or committed to report the terminal state: the owner or
	// service being destroyed leaves no reader, so a reporter that opens a
	// turn on notice would otherwise spend a model request per teardown
	// layer.
	Reported bool
}

// Read is the output and post-read state returned by Read.
type Read struct {
	// Text: stream kinds deliver the consuming delta since the previous
	// read; final-output kinds deliver empty while live and the terminal
	// output once settled — idempotent, never consumed.
	Text string
	// Snapshot is the job's state at read time.
	Snapshot Snapshot
}

// DoneListener receives each terminal snapshot and its exact owner.
type DoneListener func(snapshot Snapshot, owner Owner)

// ChangedListener observes a change to what one owner's List would return.
// It is owner-granular rather than job-granular because the change may be a
// removal, which no per-job record can express. A nil owner means an
// unowned job changed, so every caller's visible set changed with it.
type ChangedListener func(owner Owner)

// KillResult distinguishes a fresh cancellation request from a job that had
// already settled.
type KillResult string

// Kill outcomes.
const (
	KillRequested       KillResult = "requested"
	KillAlreadyFinished KillResult = "already-finished"
)

func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusKilled || status == StatusFailed
}

// tracked is the registry's mutable per-job record, never handed out.
type tracked struct {
	id               string
	kind             string
	label            string
	outputLimitBytes int
	owner            Owner
	cancel           func(reason string) error
	readOutput       func() string
	status           string
	detail           string
	output           string
	startedAt        int64
	finishedAt       int64
	reported         bool
	// settled closes on the first effective settlement.
	settled chan struct{}
	// waiters counts live waits; settlement with a waiter marks the job
	// reported.
	waiters int
	// release signals one live wait; settlement closes every channel.
	releases []chan struct{}
}

// jobLayer is one scope's contributions. Entries are anonymous because a
// contribution is identified by its own disposer, never by a name a second
// registrant could shadow.
type jobLayer struct {
	controllers scope.AnonymousEntries[struct{}]
	listeners   scope.AnonymousEntries[DoneListener]
	changed     scope.AnonymousEntries[ChangedListener]
}

// Config configures the process-local job registry.
type Config struct {
	// MaxConcurrentJobsPerOwner bounds running+stopping jobs per exact
	// owner or in the shared unowned bucket; zero defaults to 10.
	MaxConcurrentJobsPerOwner int
}

// LocalRegistry is the in-memory jobs registry.
type LocalRegistry struct {
	config                    Config
	maxConcurrentJobsPerOwner int
	logger                    Logger

	mu sync.Mutex
	// dispatchMu serializes listener and observer dispatch so concurrent
	// settlements cannot run listeners in parallel — the official host is
	// single-threaded and listeners (including in-tree ones) are written
	// against that serialization guarantee. Never held while acquiring mu.
	dispatchMu      sync.Mutex
	store           map[string]*tracked
	counters        map[string]int
	layers          map[scope.ScopeKey]*jobLayer
	listenersClosed bool
	// owners with live cleanup registration, keyed by owner id.
	owners map[string]Owner
	// starting reserves an admission slot per owner while its Run executes
	// outside the lock, so concurrent starts cannot exceed the quota.
	starting map[string]int
	now      func() time.Time
}

// Logger receives contained-listener diagnostics.
type Logger interface {
	Warn(args ...any)
}

// NewLocalRegistry builds one registry. A nil logger discards diagnostics.
func NewLocalRegistry(config Config, logger Logger) (*LocalRegistry, error) {
	if config.MaxConcurrentJobsPerOwner == 0 {
		config.MaxConcurrentJobsPerOwner = DefaultMaxConcurrentJobsPerOwner
	}
	if config.MaxConcurrentJobsPerOwner < 0 {
		return nil, fmt.Errorf("jobs: maxConcurrentJobsPerOwner must be a positive integer")
	}
	if logger == nil {
		logger = discardLogger{}
	}
	return &LocalRegistry{
		config:                    config,
		maxConcurrentJobsPerOwner: config.MaxConcurrentJobsPerOwner,
		logger:                    logger,
		store:                     map[string]*tracked{},
		counters:                  map[string]int{},
		layers:                    map[scope.ScopeKey]*jobLayer{},
		owners:                    map[string]Owner{},
		starting:                  map[string]int{},
		now:                       func() time.Time { return time.Now() },
	}, nil
}

type discardLogger struct{}

func (discardLogger) Warn(...any) {}

// Start preflights access, validation, owner cleanup, and admission, then
// starts and atomically registers the work. A rejection leaves no job id or
// execution resource; a failing Run leaves nothing registered; after Run
// returns, registration cannot fail.
func (r *LocalRegistry) Start(spec StartSpec) (string, error) {
	if !r.servesOwner(spec.Owner) {
		return "", fmt.Errorf("background jobs unavailable: no job controller serves this agent (load @deepseek-ai/dsh-tool-jobs in its composition)")
	}
	if spec.Kind == "" {
		return "", fmt.Errorf("invalid job kind: expected a non-empty string")
	}
	if spec.Label == "" {
		return "", fmt.Errorf("invalid job label: expected a non-empty string")
	}
	if spec.OutputLimitBytes < 0 {
		return "", fmt.Errorf("invalid outputLimitBytes: expected a positive safe integer, got %d", spec.OutputLimitBytes)
	}
	if spec.Run == nil {
		return "", fmt.Errorf("invalid job start: expected a run function")
	}

	r.mu.Lock()
	if spec.Owner != nil {
		r.owners[spec.Owner.OwnerID()] = spec.Owner
	}
	var active int
	for _, job := range r.store {
		if sameOwner(job.owner, spec.Owner) && (job.status == StatusRunning || job.status == StatusStopping) {
			active++
		}
	}
	ownerKey := ""
	if spec.Owner != nil {
		ownerKey = spec.Owner.OwnerID()
	}
	if active+r.starting[ownerKey] >= r.maxConcurrentJobsPerOwner {
		r.mu.Unlock()
		return "", fmt.Errorf(
			"background job limit reached for this owner (limit: %d); use job_kill to stop an unneeded job, wait for it to finish, then retry",
			r.maxConcurrentJobsPerOwner,
		)
	}
	// Reserve the slot for the unlocked Run: a concurrent start observes
	// the reservation instead of racing past the same admission count.
	r.starting[ownerKey]++
	r.mu.Unlock()

	hooks, err := spec.Run()
	if err != nil {
		r.releaseStarting(ownerKey)
		return "", err
	}

	r.mu.Lock()
	r.starting[ownerKey]--
	if r.starting[ownerKey] <= 0 {
		delete(r.starting, ownerKey)
	}
	count := r.counters[spec.Kind] + 1
	r.counters[spec.Kind] = count
	id := fmt.Sprintf("%s-%d", spec.Kind, count)
	job := &tracked{
		id:               id,
		kind:             spec.Kind,
		label:            spec.Label,
		outputLimitBytes: spec.OutputLimitBytes,
		owner:            spec.Owner,
		cancel:           hooks.Cancel,
		readOutput:       hooks.ReadOutput,
		status:           StatusRunning,
		startedAt:        r.now().UnixMilli(),
		settled:          make(chan struct{}),
	}
	r.store[id] = job
	r.mu.Unlock()

	go r.observe(job, hooks.Done)
	// Registration is complete and cannot fail from here, so the visible
	// set has genuinely changed.
	r.notifyChanged(job.owner)
	return id, nil
}

// releaseStarting drops one admission reservation after a failed Run.
func (r *LocalRegistry) releaseStarting(ownerKey string) {
	r.mu.Lock()
	r.starting[ownerKey]--
	if r.starting[ownerKey] <= 0 {
		delete(r.starting, ownerKey)
	}
	r.mu.Unlock()
}

// sameOwner compares exact owner identity by session id.
func sameOwner(left, right Owner) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.OwnerID() == right.OwnerID()
}

// observe converts one producer settlement into a registry settlement,
// containing a producer contract violation so cleanup and waiters cannot
// hang.
func (r *LocalRegistry) observe(job *tracked, done <-chan Result) {
	result, ok := <-done
	if !ok {
		// A closed Done without a result is the violation path too.
		result = Result{Err: fmt.Errorf("producer done channel closed without a result")}
	}
	if result.Err != nil {
		r.logger.Warn(fmt.Sprintf("jobs: job %s producer done promise rejected (producer contract violation): %v", job.id, result.Err))
		r.settle(job, Outcome{Status: OutcomeFailed, Detail: result.Err.Error()})
		return
	}
	r.settle(job, result.Outcome)
}

// List returns caller-owned and unowned jobs in registration order without
// exposing another session's labels. An empty caller sees only unowned
// jobs.
func (r *LocalRegistry) List(caller string) []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	ordered := make([]Snapshot, 0, len(r.store))
	for _, job := range r.store {
		if job.owner != nil && job.owner.OwnerID() != caller {
			continue
		}
		ordered = append(ordered, project(job))
	}
	return ordered
}

// Get returns a non-consuming snapshot without changing the read cursor or
// notice state. It fails loud for an unknown or foreign job.
func (r *LocalRegistry) Get(id, caller string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := r.expectLocked(id, caller)
	if err != nil {
		return Snapshot{}, err
	}
	return project(job), nil
}

// Read returns the next stream delta, or the idempotent final output after
// settlement. A terminal read marks the job reported. The producer read
// callback runs outside mu (it may touch producer internals); the reported
// mark and snapshot are committed under the lock, so a settlement racing
// the read still lands first-wins.
func (r *LocalRegistry) Read(id, caller string) (Read, error) {
	r.mu.Lock()
	job, err := r.expectLocked(id, caller)
	if err != nil {
		r.mu.Unlock()
		return Read{}, err
	}
	readOutput := job.readOutput
	if readOutput == nil {
		text := ""
		if isTerminal(job.status) {
			text = job.output
			job.reported = true
		}
		read := Read{Text: text, Snapshot: project(job)}
		r.mu.Unlock()
		return read, nil
	}
	r.mu.Unlock()
	text := readOutput()
	r.mu.Lock()
	defer r.mu.Unlock()
	if isTerminal(job.status) {
		job.reported = true
	}
	return Read{Text: text, Snapshot: project(job)}, nil
}

// Kill requests cancellation, then marks the job stopping and reported. A
// producer cancel error propagates without changing job state. Cancel runs
// outside mu; the stopping commit re-checks terminal status so a settlement
// racing the cancel keeps first-wins.
func (r *LocalRegistry) Kill(id, caller, reason string) (KillResult, error) {
	r.mu.Lock()
	job, err := r.expectLocked(id, caller)
	if err != nil {
		r.mu.Unlock()
		return "", err
	}
	if isTerminal(job.status) {
		job.reported = true
		r.mu.Unlock()
		return KillAlreadyFinished, nil
	}
	cancel := job.cancel
	r.mu.Unlock()
	if cancel != nil {
		if err := cancel(reason); err != nil {
			return "", err
		}
	}
	r.mu.Lock()
	marked := false
	if !isTerminal(job.status) {
		job.status = StatusStopping
		job.reported = true
		marked = true
	}
	owner := job.owner
	r.mu.Unlock()
	if marked {
		r.notifyChanged(owner)
	}
	return KillRequested, nil
}

// Wait blocks for settlement or the timeout without cancelling the job.
// Caller cancellation (ctx) rejects only while the job is live; a timeout
// returns the snapshot as-is; after settlement the terminal snapshot wins
// so a notice suppressed for this waiter is still delivered.
func (r *LocalRegistry) Wait(id, caller string, timeoutMs int, ctx context.Context) (Snapshot, error) {
	job, err := r.expect(id, caller)
	if err != nil {
		return Snapshot{}, err
	}
	if timeoutMs <= 0 {
		return Snapshot{}, fmt.Errorf("invalid wait timeout: expected a positive number of milliseconds, got %d", timeoutMs)
	}
	r.mu.Lock()
	if isTerminal(job.status) {
		job.reported = true
		snapshot := project(job)
		r.mu.Unlock()
		return snapshot, nil
	}
	if ctx != nil && ctx.Err() != nil {
		r.mu.Unlock()
		return Snapshot{}, fmt.Errorf("wait aborted")
	}
	release := make(chan struct{})
	job.waiters++
	counted := true
	uncount := func() {
		if !counted {
			return
		}
		counted = false
		job.waiters--
	}
	job.releases = append(job.releases, release)
	r.mu.Unlock()
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-job.settled:
	case <-release:
	case <-timer.C:
	case <-waitAbort(ctx):
		r.mu.Lock()
		uncount()
		r.mu.Unlock()
		return Snapshot{}, fmt.Errorf("wait aborted")
	}
	r.mu.Lock()
	uncount()
	if isTerminal(job.status) {
		// A settled job's terminal read marks it reported even when this
		// waiter raced the timeout.
		job.reported = true
	}
	snapshot := project(job)
	r.mu.Unlock()
	return snapshot, nil
}

// waitAbort exposes the caller cancellation channel for the select.
func waitAbort(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// OnJobDoneIn registers an effect-scoped completion listener filing into the
// given scope (nil = global). It receives the settlements of the owners its
// registering scope covers; each listener is contained. The returned disposer
// unregisters the listener.
func (r *LocalRegistry) OnJobDoneIn(regScope scope.ScopeKey, listener DoneListener) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	layer := r.layerLocked(regScope)
	undo := layer.listeners.Append(listener)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		undo()
	}
}

// OnJobsChangedIn registers an effect-scoped observer of visible-set changes
// filing into the given scope (nil = global). It fires after every commit
// that changes what List returns for that owner — registration, every
// stopping transition, settlement, owner-disposal removal, and the emptying
// that service disposal commits. This is not a superset of OnJobDone: it
// carries no delivery meaning and marks nothing reported.
func (r *LocalRegistry) OnJobsChangedIn(regScope scope.ScopeKey, listener ChangedListener) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	layer := r.layerLocked(regScope)
	undo := layer.changed.Append(listener)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		undo()
	}
}

// AttachControllerIn attaches an effect-scoped controller that can read and
// stop jobs, filing into the given scope (nil = global). Start refuses an
// owner no attached controller serves.
func (r *LocalRegistry) AttachControllerIn(regScope scope.ScopeKey) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	layer := r.layerLocked(regScope)
	undo := layer.controllers.Append(struct{}{})
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		undo()
	}
}

// layerLocked returns (creating) the layer for one scope. Callers hold mu.
func (r *LocalRegistry) layerLocked(key scope.ScopeKey) *jobLayer {
	layer := r.layers[key]
	if layer == nil {
		layer = &jobLayer{}
		r.layers[key] = layer
	}
	return layer
}

// servesOwner reports whether an attached job controller can collect and
// stop work owned by owner. The global layer holds every controller attached
// from an unscoped context and therefore serves every owner; a scoped
// controller serves exactly the agents composed under it.
func (r *LocalRegistry) servesOwner(owner Owner) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if global := r.layers[nil]; global != nil && !global.controllers.IsEmpty() {
		return true
	}
	var chain []scope.ScopeKey
	if owner != nil {
		chain = scope.ChainOf(owner.OwnerScope())
	}
	for _, key := range chain {
		if layer := r.layers[key]; layer != nil && !layer.controllers.IsEmpty() {
			return true
		}
	}
	return false
}

// expect looks up a job or fails loud; assertAccess runs beside it.
func (r *LocalRegistry) expect(id, caller string) (*tracked, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expectLocked(id, caller)
}

// expectLocked is expect without locking. Callers hold mu.
func (r *LocalRegistry) expectLocked(id, caller string) (*tracked, error) {
	job := r.store[id]
	if job == nil {
		return nil, fmt.Errorf("unknown job %s", id)
	}
	// The isolation fence: a job with an owner is reachable only by
	// callers whose session id matches (an unowned job is open, and an
	// empty caller can never match an owned one).
	if job.owner != nil && job.owner.OwnerID() != caller {
		return nil, fmt.Errorf("job %s belongs to another session", job.id)
	}
	return job, nil
}

// project snapshots a fresh read-only view from the mutable record. Callers
// hold mu.
func project(job *tracked) Snapshot {
	snapshot := Snapshot{
		ID:        job.id,
		Kind:      job.kind,
		Label:     job.label,
		Status:    job.status,
		StartedAt: job.startedAt,
		Reported:  job.reported,
	}
	if job.outputLimitBytes > 0 {
		snapshot.OutputLimitBytes = job.outputLimitBytes
	}
	if job.owner != nil {
		snapshot.OwnerSession = job.owner.OwnerID()
	}
	if job.detail != "" {
		snapshot.Detail = job.detail
	}
	if job.finishedAt != 0 {
		snapshot.FinishedAt = job.finishedAt
	}
	return snapshot
}

// listenersFor yields the completion listeners that own owner's notices:
// the global layer's first, then each scoped layer along the owner's chain.
// A listener outside that chain belongs to another composition and must not
// deliver. Callers hold mu.
func (r *LocalRegistry) listenersFor(owner Owner) []DoneListener {
	listeners := make([]DoneListener, 0)
	if global := r.layers[nil]; global != nil {
		listeners = append(listeners, global.listeners.Values()...)
	}
	var chain []scope.ScopeKey
	if owner != nil {
		chain = scope.ChainOf(owner.OwnerScope())
	}
	for _, key := range chain {
		if layer := r.layers[key]; layer != nil {
			listeners = append(listeners, layer.listeners.Values()...)
		}
	}
	return listeners
}

// changedFor resolves the change observers that own owner's updates, exactly
// like listenersFor. Callers hold mu.
func (r *LocalRegistry) changedFor(owner Owner) []ChangedListener {
	listeners := make([]ChangedListener, 0)
	if global := r.layers[nil]; global != nil {
		listeners = append(listeners, global.changed.Values()...)
	}
	var chain []scope.ScopeKey
	if owner != nil {
		chain = scope.ChainOf(owner.OwnerScope())
	}
	for _, key := range chain {
		if layer := r.layers[key]; layer != nil {
			listeners = append(listeners, layer.changed.Values()...)
		}
	}
	return listeners
}

// notifyChanged announces that one owner's visible set changed. Each
// listener is contained so an observer cannot break a lifecycle commit that
// already happened.
func (r *LocalRegistry) notifyChanged(owner Owner) {
	r.mu.Lock()
	observers := r.changedFor(owner)
	r.mu.Unlock()
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	for _, listener := range observers {
		r.contain(func() { listener(owner) }, "jobs: onJobsChanged listener threw: %v")
	}
}

// contain runs one listener, logging instead of propagating its panic.
func (r *LocalRegistry) contain(run func(), warnFormat string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Warn(fmt.Sprintf(warnFormat, recovered))
		}
	}()
	run()
}

// settle records the first terminal outcome, releases waiters, then
// announces completion. First-wins preserves a teardown force-failure
// against late producer settlement. Pending waits mark the job reported
// before listeners run. Completion is announced last because a reporter may
// open a model turn synchronously: every other observer of this settlement
// must already have seen the committed record.
func (r *LocalRegistry) settle(job *tracked, outcome Outcome) {
	r.mu.Lock()
	if isTerminal(job.status) {
		r.mu.Unlock()
		return
	}
	job.status = outcome.Status
	job.detail = outcome.Detail
	job.output = outcome.Output
	job.finishedAt = r.now().UnixMilli()
	if job.waiters > 0 {
		job.reported = true
	}
	snapshot := project(job)
	releases := job.releases
	job.releases = nil
	close(job.settled)
	r.mu.Unlock()
	for _, release := range releases {
		close(release)
	}
	r.notifyChanged(job.owner)

	r.mu.Lock()
	closed := r.listenersClosed
	listeners := r.listenersFor(job.owner)
	r.mu.Unlock()
	if closed {
		return
	}
	owner := job.owner
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	for _, listener := range listeners {
		r.contain(func() { listener(snapshot, owner) }, "jobs: onJobDone listener threw for "+job.id+": %v")
	}
}

// DisposeOwner cancels, awaits terminal records, and drops every job owned
// by one exact agent lifecycle. The agent lifecycle calls this on disposal
// (Go adaptation of the official scoped effect). Removal is the one
// visible-set change no per-job record carries, so it is announced here.
func (r *LocalRegistry) DisposeOwner(owner Owner) {
	r.mu.Lock()
	owned := make([]*tracked, 0)
	for _, job := range r.store {
		if sameOwner(job.owner, owner) {
			owned = append(owned, job)
		}
	}
	r.mu.Unlock()
	r.cancelForTeardown(owned, "owner disposed")
	for _, job := range owned {
		<-job.settled
	}
	r.mu.Lock()
	for _, job := range owned {
		delete(r.store, job.id)
	}
	if owner != nil {
		delete(r.owners, owner.OwnerID())
	}
	r.mu.Unlock()
	if len(owned) > 0 {
		r.notifyChanged(owner)
	}
}

// Dispose closes listeners, cancels live jobs, awaits settlement, and
// empties the store: the service teardown effect. Throwing cancels are
// force-failed to avoid teardown deadlock.
func (r *LocalRegistry) Dispose() {
	// The flag is the whole guard: listener disposers belong to the fibers
	// that registered them, so this service does not drop them on its own
	// way out.
	r.mu.Lock()
	r.listenersClosed = true
	all := make([]*tracked, 0, len(r.store))
	for _, job := range r.store {
		all = append(all, job)
	}
	r.mu.Unlock()
	r.cancelForTeardown(all, "jobs service disposed")
	for _, job := range all {
		<-job.settled
	}
	r.mu.Lock()
	emptied := map[string]Owner{}
	for _, job := range all {
		if job.owner != nil {
			emptied[job.owner.OwnerID()] = job.owner
		} else {
			emptied[""] = nil
		}
	}
	r.store = map[string]*tracked{}
	r.owners = map[string]Owner{}
	r.mu.Unlock()
	for _, owner := range emptied {
		r.notifyChanged(owner)
	}
}

// cancelForTeardown cancels jobs during teardown with per-job containment.
// A throwing cancel force-fails the record and reports a possible orphan; a
// cancel that returns without settling remains indistinguishable from a
// slow stop and may stall.
func (r *LocalRegistry) cancelForTeardown(jobs []*tracked, reason string) {
	for _, job := range jobs {
		r.mu.Lock()
		if isTerminal(job.status) {
			r.mu.Unlock()
			continue
		}
		// Teardown cancellation is a kill without a caller, so it claims
		// the terminal report the same way Kill does — decided before the
		// producer runs, because the force-failure below settles the
		// record too and a throwing cancel must not be the one path that
		// announces an unreported completion into a disposing owner.
		job.reported = true
		cancel := job.cancel
		r.mu.Unlock()
		if cancel != nil {
			if err := cancel(reason); err != nil {
				detail := fmt.Sprintf("cancel threw during teardown; work may be orphaned: %v", err)
				r.logger.Warn(fmt.Sprintf("jobs: cancel of %s threw during teardown; job record forced failed and work may be orphaned: %v", job.id, err))
				r.settle(job, Outcome{Status: OutcomeFailed, Detail: detail})
				continue
			}
		}
		r.mu.Lock()
		job.status = StatusStopping
		r.mu.Unlock()
		// Announcing the transition here is what keeps an observer from
		// showing running for the whole window until the producer
		// releases.
		r.notifyChanged(job.owner)
	}
}
