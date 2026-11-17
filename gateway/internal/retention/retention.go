// Package retention keeps the database small (ARCHITECTURE.md §13.1): it
// deletes request logs past logs.retention_days, trims the oldest logs when
// they outgrow logs.max_size_mb, keeps PostgreSQL's daily partitions ahead of
// time, and drops leftover state of keys deleted from the config.
//
// It runs shortly after start and then every hour, so each pass deletes about
// an hour of logs: small batches, short locks.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const (
	// DefaultInterval is the time between two passes.
	DefaultInterval = time.Hour
	// removedKeyRetention: state of a key deleted from keys/*.yaml is kept this
	// long, so re-adding the key restores its usage and status (§14.5).
	removedKeyRetention = 30 * 24 * time.Hour
	// trimTarget: a trim brings the logs down to this share of the cap, so the
	// next few hours of traffic do not trigger it again right away.
	trimTarget = 0.9
)

// Store is the part of *store.Store the job uses.
type Store interface {
	PurgeRequestLogsBefore(ctx context.Context, cutoff time.Time) (int64, error)
	LogsSize(ctx context.Context) (int64, error)
	RequestLogStats(ctx context.Context) (store.LogStats, error)
	NthOldestLogTS(ctx context.Context, n int64) (time.Time, error)
	EnsureLogPartitions(ctx context.Context, now time.Time) error
	PurgeRemovedKeys(ctx context.Context, cutoff time.Time) (int64, error)
}

// Options wires a Job.
type Options struct {
	Store    Store
	Snapshot func() *confstore.Snapshot
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration // 0 = DefaultInterval
	Delay    time.Duration // first pass after start (0 = 1 minute, negative = immediately)
}

// Status is what the last pass found and did, for the panel.
type Status struct {
	At           time.Time // zero before the first pass
	SizeBytes    int64     // after the pass
	MaxBytes     int64
	Rows         int64
	Oldest       time.Time
	AgeDeleted   int64 // logs past the retention period
	SizeDeleted  int64 // oldest logs removed for the size cap
	KeysPurged   int64
	RetentionDay int
	Err          string
}

// Job is safe for concurrent use.
type Job struct {
	o    Options
	log  *slog.Logger
	mu   sync.Mutex
	last Status
}

// New returns a Job; call Run to start it.
func New(o Options) *Job {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	switch {
	case o.Delay == 0:
		o.Delay = time.Minute
	case o.Delay < 0:
		o.Delay = 0
	}
	return &Job{o: o, log: o.Logger}
}

// Status returns the result of the last pass.
func (j *Job) Status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last
}

// Run passes every interval until ctx is done.
func (j *Job) Run(ctx context.Context) {
	t := time.NewTimer(j.o.Delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		j.Pass(ctx)
		t.Reset(j.o.Interval)
	}
}

// Pass runs one round of maintenance and returns what it did.
func (j *Job) Pass(ctx context.Context) Status {
	cfg := j.o.Snapshot().Config.Logs
	now := j.o.Now()
	st := Status{At: now, MaxBytes: int64(cfg.MaxSizeMB) << 20, RetentionDay: cfg.RetentionDays}
	var errs []error
	fail := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			errs = append(errs, err)
			j.log.Warn("log retention: "+what+" failed", "err", err)
		}
	}

	// Create upcoming partitions before anything else: without them new logs
	// pile up in the default partition, which can only be deleted row by row.
	fail("partition maintenance", j.o.Store.EnsureLogPartitions(ctx, now))

	n, err := j.o.Store.PurgeRequestLogsBefore(ctx, now.Add(-time.Duration(cfg.RetentionDays)*24*time.Hour))
	st.AgeDeleted = n
	fail("purge by age", err)

	n, err = j.trim(ctx, st.MaxBytes)
	st.SizeDeleted = n
	fail("size cap", err)

	n, err = j.o.Store.PurgeRemovedKeys(ctx, now.Add(-removedKeyRetention))
	st.KeysPurged = n
	fail("purge removed keys", err)

	if size, err := j.o.Store.LogsSize(ctx); err == nil {
		st.SizeBytes = size
	} else {
		fail("measure size", err)
	}
	if ls, err := j.o.Store.RequestLogStats(ctx); err == nil {
		st.Rows, st.Oldest = ls.Rows, ls.Oldest
	} else {
		fail("count logs", err)
	}
	if err := errors.Join(errs...); err != nil {
		st.Err = err.Error()
	}
	if st.AgeDeleted+st.SizeDeleted+st.KeysPurged > 0 {
		j.log.Info("log retention", "deleted_by_age", st.AgeDeleted, "deleted_for_size", st.SizeDeleted,
			"removed_keys_purged", st.KeysPurged, "size_mb", st.SizeBytes>>20, "rows", st.Rows)
	}
	j.mu.Lock()
	j.last = st
	j.mu.Unlock()
	return st
}

// trim deletes the oldest logs when they take more than maxBytes, aiming at
// trimTarget of the cap. The rows to delete are estimated from the average
// row size; one pass per run is enough (PostgreSQL only returns the space of
// deleted rows after a vacuum, so re-measuring at once would over-delete).
func (j *Job) trim(ctx context.Context, maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return 0, nil
	}
	size, err := j.o.Store.LogsSize(ctx)
	if err != nil || size <= maxBytes {
		return 0, err
	}
	ls, err := j.o.Store.RequestLogStats(ctx)
	if err != nil || ls.Rows == 0 {
		return 0, err
	}
	share := 1 - trimTarget*float64(maxBytes)/float64(size)
	n := int64(math.Ceil(share * float64(ls.Rows)))
	cut, err := j.o.Store.NthOldestLogTS(ctx, min(max(n, 1), ls.Rows))
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	j.log.Warn("request logs exceed logs.max_size_mb: deleting the oldest", "size_mb", size>>20, "max_mb", maxBytes>>20, "rows", n)
	return j.o.Store.PurgeRequestLogsBefore(ctx, cut.Add(time.Millisecond))
}
