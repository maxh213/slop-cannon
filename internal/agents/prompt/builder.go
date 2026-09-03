package prompt

import (
	"fmt"
	"strings"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Section is one caller-supplied block of task-specific instructions: what
// this particular firing must do, over and above the role's base prompt.
// P5-02, P5-03 and P5-04 own the wording; this package only places it.
type Section struct {
	// Title is the "## " heading. An empty title appends the body to the
	// previous section instead of opening a new one.
	Title string
	// Body is Markdown, rendered verbatim.
	Body string
}

// Request is everything one prompt is assembled from. Only Role is
// mandatory; every other field contributes a layer when it is set. See the
// package documentation for the order the layers are rendered in.
type Request struct {
	// Role selects the base template and how the agent is addressed.
	Role domain.Role
	// Lens is required for testers and must be empty for anybody else.
	Lens domain.Lens
	// Task is the job the agent was fired for (events.TaskImplement,
	// events.TaskReview …). It names the prompt and is echoed to the agent.
	Task events.Task
	// AgentID identifies the agent; it is also the name of the debug copy
	// written by Write.
	AgentID domain.AgentID
	// RunID identifies the run.
	RunID domain.RunID
	// ToolPolicy is the policy the firing runs under; set it with
	// agents.ApplyPolicy and pass the same value here so the prompt and the
	// cartridge flags agree.
	ToolPolicy cartridge.ToolPolicy
	// Objective is one or two sentences saying what this firing must
	// achieve. It appears in the assignment block.
	Objective string
	// Brief is the human's -m text, quoted verbatim; empty in autopilot.
	Brief string
	// Rules are hard constraints, rendered verbatim as MUST (D-04 rules).
	Rules []Doc
	// Vibes are style preferences, rendered verbatim as preferences.
	Vibes []Doc
	// Repo describes the repository; see ScanRepo.
	Repo RepoContext
	// OpenBeads are the beads that already exist, so the product owner does
	// not propose them again (P5-02).
	OpenBeads []domain.Bead
	// Bead is the bead this agent owns; nil for a planning product owner.
	Bead *BeadContext
	// History is the feedback loop so far, oldest round first.
	History []Round
	// Instructions are the task-specific sections, rendered after the
	// context and before the output contract.
	Instructions []Section
	// Contract is the structured-output contract the agent must satisfy
	// (agents.ContractFor). When nil no output contract is appended, which
	// is only right for a firing that produces no structured message.
	Contract *structured.Contract
	// Budget bounds the assembled prompt; the zero value means the defaults.
	Budget Budget
}

// Prompt is an assembled prompt and what had to be left out of it.
type Prompt struct {
	// Text is the prompt itself, ending in exactly one newline.
	Text string
	// Role, Lens, Task and AgentID are copied from the request so a caller
	// can log or file the prompt without carrying it around.
	Role    domain.Role
	Lens    domain.Lens
	Task    events.Task
	AgentID domain.AgentID
	// Tokens is the estimated size of Text (see Budget.Estimate).
	Tokens int
	// MaxTokens is the budget it was measured against.
	MaxTokens int
	// Truncations describes, in the order they were applied, everything the
	// budget forced out of the prompt. Empty when nothing was dropped.
	Truncations []string
	// OverBudget is true when even the most degraded prompt is larger than
	// the budget, which means the rules, vibes and contract alone exceed it.
	// The prompt is still usable; the caller decides whether to fire it.
	OverBudget bool
	// Path is where Write put the debug copy, once it has.
	Path string
}

// Fits reports whether the prompt is within its budget.
func (p Prompt) Fits() bool { return !p.OverBudget }

// String returns the prompt text, so a Prompt can be passed where a
// fmt.Stringer is wanted.
func (p Prompt) String() string { return p.Text }

// Build assembles the prompt for one firing. It fails only on an
// unusable request: an unknown role, a tester without a lens (or a
// non-tester with one), or a missing template.
func Build(req Request) (Prompt, error) {
	base, err := basePrompt(req)
	if err != nil {
		return Prompt{}, err
	}
	budget := req.Budget.withDefaults()
	full := fullKnobs(req)

	var (
		text  string
		knobs = full
		fits  bool
	)
	for _, k := range degradations(req, full) {
		knobs = k
		text = render(req, base, k)
		if budget.Estimate(text) <= budget.MaxTokens {
			fits = true
			break
		}
	}

	p := Prompt{
		Text:        text,
		Role:        req.Role,
		Lens:        req.Lens,
		Task:        req.Task,
		AgentID:     req.AgentID,
		Tokens:      budget.Estimate(text),
		MaxTokens:   budget.MaxTokens,
		Truncations: describeTruncations(req, full, knobs),
		OverBudget:  !fits,
	}
	return p, nil
}

// MustBuild is Build for tests and package-level values: it panics when the
// request is unusable.
func MustBuild(req Request) Prompt {
	p, err := Build(req)
	if err != nil {
		panic(err)
	}
	return p
}

// basePrompt returns the role template plus, for a tester, its lens
// template, and checks the role/lens combination.
func basePrompt(req Request) (string, error) {
	role, err := RoleTemplate(req.Role)
	if err != nil {
		return "", err
	}
	if req.Role == domain.RoleTester {
		if !req.Lens.Valid() {
			return "", fmt.Errorf("%w: a tester prompt needs a lens (one of %s)", domain.ErrUnknownValue, joinLenses())
		}
		lens, err := LensTemplate(req.Lens)
		if err != nil {
			return "", err
		}
		return role + "\n\n" + lens, nil
	}
	if req.Lens != "" {
		return "", fmt.Errorf("%w: lens %q on a %s prompt; only testers have a lens",
			domain.ErrUnknownValue, string(req.Lens), req.Role)
	}
	return role, nil
}

// joinLenses lists the lenses for error messages.
func joinLenses() string {
	all := domain.Lenses()
	parts := make([]string, len(all))
	for i, l := range all {
		parts[i] = l.String()
	}
	return strings.Join(parts, ", ")
}

// render assembles the whole prompt for one set of degradation knobs. It is
// deterministic: the same request and knobs always produce the same bytes.
func render(req Request, base string, k knobs) string {
	var b strings.Builder
	b.WriteString("# " + title(req) + "\n\n" + base + "\n")

	writeSection(&b, "Your assignment", renderAssignment(req))
	writeSection(&b, "Brief from the human", strings.TrimSpace(req.Brief))
	writeSection(&b, "Rules (MUST)", renderDocs(req.Rules,
		"These are hard constraints for this repository. You MUST follow every one of them. "+
			"Breaking a rule is a blocking review failure, and a tester who finds one must report it as a blocker."))
	writeSection(&b, "Vibes (preferences)", renderDocs(req.Vibes,
		"These are the humans' stylistic preferences. Follow them unless a rule or the bead says otherwise; "+
			"they are guidance, not law."))
	writeSection(&b, "Repository context", renderRepo(req.Repo, k.tree, k.readme))
	writeSection(&b, "Open beads", renderOpenBeads(req.OpenBeads, k.openBeads))
	writeSection(&b, "Your bead", renderBead(req.Bead))
	writeSection(&b, "Round history", renderHistory(req.History, k.rounds))
	for _, s := range req.Instructions {
		writeSection(&b, s.Title, strings.TrimSpace(s.Body))
	}

	text := strings.TrimRight(b.String(), "\n") + "\n"
	if req.Contract != nil {
		text = req.Contract.WithPromptSuffix(text)
	}
	return strings.TrimRight(text, "\n") + "\n"
}

// writeSection appends "## title" and body, skipping empty bodies. A
// section with no title appends the body on its own.
func writeSection(b *strings.Builder, title, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	if strings.TrimSpace(title) != "" {
		b.WriteString("\n## " + strings.TrimSpace(title) + "\n")
	}
	b.WriteString("\n" + strings.TrimRight(body, "\n") + "\n")
}

// title names the prompt: "Engineer — implement", "Tester (qa) — review".
func title(req Request) string {
	name := roleTitles[req.Role]
	if name == "" {
		name = strings.ToUpper(req.Role.String()[:1]) + req.Role.String()[1:]
	}
	if req.Role == domain.RoleTester && req.Lens != "" {
		name += " (" + req.Lens.String() + ")"
	}
	if req.Task != "" {
		name += " — " + strings.ReplaceAll(req.Task.String(), "_", " ")
	}
	return name
}

// roleTitles is how each role is named in the prompt heading.
var roleTitles = map[domain.Role]string{
	domain.RolePO:       "Product owner",
	domain.RoleEngineer: "Engineer",
	domain.RoleTester:   "Tester",
}

// policyLines explain each tool policy in the agent's own terms. The
// wording matches what the adapter actually enforces (P5-06).
var policyLines = map[cartridge.ToolPolicy]string{
	cartridge.ToolReadOnly: "read_only — you may read and search files. You must not edit, create or delete anything, " +
		"and the tools you have will refuse if you try.",
	cartridge.ToolReadExec: "read_exec — you may read files and run commands (tests, builds, linters). " +
		"You must not edit, create or delete files; the tools you have will refuse if you try.",
	cartridge.ToolReadWrite: "read_write — you may read files, run commands and edit files inside your working directory. " +
		"Nothing outside it is yours to touch.",
}

// renderAssignment renders the identity and ground rules of this firing.
func renderAssignment(req Request) string {
	var b strings.Builder
	if o := strings.TrimSpace(req.Objective); o != "" {
		b.WriteString(o + "\n\n")
	}
	writeBullet(&b, "Run", string(req.RunID))
	writeBullet(&b, "Agent", string(req.AgentID))
	writeBullet(&b, "Role", req.Role.String())
	if req.Lens != "" {
		writeBullet(&b, "Lens", req.Lens.String())
	}
	if req.Task != "" {
		writeBullet(&b, "Task", req.Task.String())
	}
	if line := policyLines[req.ToolPolicy]; line != "" {
		writeBullet(&b, "Tool policy", line)
	}
	b.WriteString("- " + beads.PromptNote + "\n")
	b.WriteString("- Everything you write outside the final result block is read by humans watching the run, " +
		"so keep it short and say what you are doing.\n")
	return strings.TrimRight(b.String(), "\n")
}
