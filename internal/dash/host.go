package dash

import (
	"context"
	"time"
)

const maxHostCores = 256

type hostSampleFunc func() (HostSnapshot, error)

type hostEvent struct {
	at      time.Time
	sample  HostSnapshot
	success bool
}

func (o *Observer) startHostCollection(ctx context.Context) (<-chan hostEvent, <-chan struct{}) {
	events := make(chan hostEvent, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.collectHost(ctx, events)
	}()
	return events, done
}

func (o *Observer) collectHost(ctx context.Context, events chan<- hostEvent) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		sample, err := o.sampleHost()
		if !sendHostEvent(ctx, events, hostEvent{at: o.now().UTC(), sample: sample, success: err == nil}) {
			return
		}
		timer.Reset(o.opts.HostRefreshInterval)
	}
}

func sendHostEvent(ctx context.Context, events chan<- hostEvent, event hostEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (o *Observer) applyHostEvent(event hostEvent) {
	if !event.success {
		return
	}
	sample := cloneHostSnapshot(event.sample)
	sample.CollectedAt = event.at.UTC()
	boundHostCores(&sample)
	o.host = &sample
}

func boundHostCores(sample *HostSnapshot) {
	if sample.CPU == nil {
		return
	}
	sample.CPU.CoresOmitted = max(len(sample.CPU.Cores)-maxHostCores, 0)
	if len(sample.CPU.Cores) > maxHostCores {
		sample.CPU.Cores = sample.CPU.Cores[:maxHostCores]
	}
}

func cloneHostSnapshot(snapshot HostSnapshot) HostSnapshot {
	if snapshot.CPU != nil {
		cpu := *snapshot.CPU
		cpu.Cores = cloneSlice(cpu.Cores)
		snapshot.CPU = &cpu
	}
	if snapshot.Swap != nil {
		swap := *snapshot.Swap
		snapshot.Swap = &swap
	}
	return snapshot
}
