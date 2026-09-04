package events

import "slop-cannon/internal/domain"

// TextFunc rewrites one piece of untrusted text: a secret redactor, an
// escape-sequence scrubber, or both through [Chain]. It must be pure — it is
// called once per field and its result replaces the field — and it must
// tolerate an empty string.
type TextFunc func(string) string

// Chain returns a [TextFunc] applying each f in order, skipping the nil
// ones. Chain of nothing returns nil, which [Redacted] treats as "leave the
// event alone".
func Chain(fs ...TextFunc) TextFunc {
	kept := make([]TextFunc, 0, len(fs))
	for _, f := range fs {
		if f != nil {
			kept = append(kept, f)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return func(s string) string {
		for _, f := range kept {
			s = f(s)
		}
		return s
	}
}

// Redacted returns a copy of ev with every piece of text that came from
// outside sc passed through f, and everything sc itself minted — ids, kinds,
// states, counts, timestamps — left exactly as it was.
//
// [Bus] deliberately never transforms what it carries: one subscriber must
// not be able to change what another sees, and the JSONL run log has to be a
// faithful record of what was published. Redaction therefore belongs at the
// construction site, where the orchestrator, the supervisor and the
// cartridge adapters build an event out of a subprocess's bytes, and this is
// the one helper they all call — see logging.SafeEvent for the composed hook
// (P9-09, P7-17). Applying it twice is harmless as long as f is idempotent,
// which both the secret redactor and the escape scrubber are.
//
// The fields it rewrites are the free text a model, a cartridge, bd, git or
// gh wrote:
//
//   - the reason of a pause, resume, stop or state change, and a run's
//     summary and brief;
//   - a plan's brief and its beads' titles, descriptions and acceptance
//     criteria, and the same fields of every bead in a create, update or
//     graph event;
//   - an agent's output line, the bead title it was fired at, and the error
//     text of a failure;
//   - a verdict's summary and its items, a response's rationale, an
//     implementation report, an arbitration's instruction and rationale and
//     an escalation's notes;
//   - a pull request's URL, branch and base, a merge's branch;
//   - the source, message and error of a warning or an error.
//
// A nil event or a nil f returns the input unchanged, and an event kind with
// no untrusted text is returned as it came in.
func Redacted(ev Event, f TextFunc) Event {
	if ev == nil || f == nil {
		return ev
	}
	switch e := ev.(type) {
	case RunStarted:
		e.Info.Brief = f(e.Info.Brief)
		return e
	case RunPaused:
		e.Reason = f(e.Reason)
		return e
	case RunResumed:
		e.Reason = f(e.Reason)
		return e
	case RunStopped:
		e.Reason = f(e.Reason)
		return e
	case RunFinished:
		e.Summary = f(e.Summary)
		return e
	case PlanProduced:
		e.Plan = redactPlan(e.Plan, f)
		return e
	case BeadCreated:
		e.Bead = redactBead(e.Bead, f)
		return e
	case BeadUpdated:
		e.Bead = redactBead(e.Bead, f)
		return e
	case BeadStateChanged:
		e.Reason = f(e.Reason)
		return e
	case GraphChanged:
		e.Beads = redactSlice(e.Beads, func(b domain.Bead) domain.Bead { return redactBead(b, f) })
		return e
	case AgentSpawned:
		e.BeadTitle = f(e.BeadTitle)
		return e
	case AgentOutput:
		e.Line = f(e.Line)
		return e
	case AgentFailed:
		e.Err = f(e.Err)
		return e
	case VerdictReceived:
		e.Verdict = redactVerdict(e.Verdict, f)
		return e
	case ResponseReceived:
		e.Responses = redactSlice(e.Responses, func(r domain.Response) domain.Response {
			r.Rationale = f(r.Rationale)
			return r
		})
		if e.Report != nil {
			report := redactReport(*e.Report, f)
			e.Report = &report
		}
		return e
	case ArbitrationReceived:
		e.Rulings = redactSlice(e.Rulings, func(a domain.Arbitration) domain.Arbitration {
			a.Instruction, a.Rationale = f(a.Instruction), f(a.Rationale)
			return a
		})
		if e.Escalation != nil {
			esc := *e.Escalation
			esc.Notes, esc.NewAcceptance, esc.Summary = f(esc.Notes), f(esc.NewAcceptance), f(esc.Summary)
			e.Escalation = &esc
		}
		return e
	case PROpened:
		e.URL, e.Branch, e.Base = f(e.URL), f(e.Branch), f(e.Base)
		return e
	case Merged:
		e.Branch = f(e.Branch)
		return e
	case Warning:
		e.Source, e.Message, e.Err = f(e.Source), f(e.Message), f(e.Err)
		return e
	case Error:
		e.Source, e.Message, e.Err = f(e.Source), f(e.Message), f(e.Err)
		return e
	default:
		return ev
	}
}

// redactBead rewrites the text a PO or a tester wrote into a bead. The id,
// state, labels and numbers are sc's own bookkeeping and are left alone.
func redactBead(b domain.Bead, f TextFunc) domain.Bead {
	b.Title, b.Description, b.Acceptance = f(b.Title), f(b.Description), f(b.Acceptance)
	b.Assignee, b.PRURL = f(b.Assignee), f(b.PRURL)
	return b
}

// redactPlan rewrites a plan's brief and every proposed bead, on a copy of
// the bead slice so the caller's plan is untouched.
func redactPlan(p domain.Plan, f TextFunc) domain.Plan {
	p.Brief = f(p.Brief)
	p.Beads = redactSlice(p.Beads, func(b domain.PlanBead) domain.PlanBead {
		b.Title, b.Description, b.Acceptance = f(b.Title), f(b.Description), f(b.Acceptance)
		return b
	})
	return p
}

// redactVerdict rewrites a tester's prose: the summary and every item's
// title, detail, suggestion, category and file path.
func redactVerdict(v domain.Verdict, f TextFunc) domain.Verdict {
	v.Summary = f(v.Summary)
	v.Items = redactSlice(v.Items, func(it domain.FeedbackItem) domain.FeedbackItem {
		it.Title, it.Detail, it.Suggestion = f(it.Title), f(it.Detail), f(it.Suggestion)
		it.Category, it.File = f(it.Category), f(it.File)
		return it
	})
	return v
}

// redactReport rewrites an implementation report, whose every field but the
// commit sha is prose the engineer wrote.
func redactReport(r domain.ImplReport, f TextFunc) domain.ImplReport {
	r.Summary, r.HowToVerify = f(r.Summary), f(r.HowToVerify)
	r.FilesChanged = redactSlice(r.FilesChanged, f)
	r.Risks, r.Questions = redactSlice(r.Risks, f), redactSlice(r.Questions, f)
	return r
}

// redactSlice returns a new slice with fn applied to every element, or nil
// for an empty input. The copy is what keeps [Redacted] from writing through
// the caller's backing array, which another subscriber may still be reading.
func redactSlice[T any](in []T, fn func(T) T) []T {
	if len(in) == 0 {
		return in
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}
