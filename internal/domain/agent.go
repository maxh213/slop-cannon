package domain

import "time"

// AgentID identifies one agent subprocess within a run, for example
// "eng-sc-68s-r2" or "tester-qa-sc-68s-r2". It is also the bd actor
// ("sc:<agent-id>") under which the orchestrator records that agent's
// messages (D-06).
type AgentID string

// String returns the id.
func (id AgentID) String() string { return string(id) }

// AgentState is the lifecycle state of an agent subprocess.
type AgentState string

// Agent lifecycle states.
const (
	// AgentStatePending means the agent is scheduled but not yet spawned.
	AgentStatePending AgentState = "pending"
	// AgentStateRunning means the subprocess is alive.
	AgentStateRunning AgentState = "running"
	// AgentStateFinished means it exited successfully with a parsed result.
	AgentStateFinished AgentState = "finished"
	// AgentStateFailed means it crashed, timed out or produced unusable output.
	AgentStateFailed AgentState = "failed"
	// AgentStateKilled means the orchestrator stopped it deliberately.
	AgentStateKilled AgentState = "killed"
)

var agentStateEnum = enum[AgentState]{kind: "agent state", values: []AgentState{
	AgentStatePending, AgentStateRunning, AgentStateFinished, AgentStateFailed, AgentStateKilled,
}}

// AgentStates returns every valid AgentState in lifecycle order.
func AgentStates() []AgentState { return agentStateEnum.all() }

// ParseAgentState parses a state name leniently; see ParseRole for the rules.
func ParseAgentState(s string) (AgentState, error) { return agentStateEnum.parse(s) }

// String returns the canonical name.
func (s AgentState) String() string { return string(s) }

// Valid reports whether s is one of the declared states.
func (s AgentState) Valid() bool { return agentStateEnum.valid(s) }

// Terminal reports whether the agent is no longer running and will not be.
func (s AgentState) Terminal() bool {
	switch s {
	case AgentStateFinished, AgentStateFailed, AgentStateKilled:
		return true
	default:
		return false
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s AgentState) MarshalText() ([]byte, error) { return agentStateEnum.marshal(s) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *AgentState) UnmarshalText(b []byte) error {
	v, err := agentStateEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// Agent is one LLM CLI subprocess doing a job for the run. It occupies one
// unit of ammo while running (D-05).
type Agent struct {
	// ID identifies the agent within the run.
	ID AgentID `json:"id"`
	// Role is what the agent does.
	Role Role `json:"role"`
	// Lens is set for testers only.
	Lens Lens `json:"lens,omitempty"`
	// Cartridge is the name of the LLM CLI adapter that runs it.
	Cartridge string `json:"cartridge"`
	// Model is the model the cartridge was asked to use, if any.
	Model string `json:"model,omitempty"`
	// Bead is the bead the agent is working on; empty for a planning PO.
	Bead BeadID `json:"bead,omitempty"`
	// State is the lifecycle state.
	State AgentState `json:"state"`
	// StartedAt is when the subprocess was spawned; zero while pending.
	StartedAt time.Time `json:"started_at"`
	// PID is the operating-system process id; zero while pending or after exit.
	PID int `json:"pid,omitempty"`
}
