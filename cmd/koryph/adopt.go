// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/adopt"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/onboard"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/sysdeps"
)

func init() {
	registerCmd(command{
		name:    "adopt",
		summary: "wizard: take an existing repo to a green `koryph validate` in one run",
		run:     cmdAdopt,
		DocLinks: []string{
			"user-guide/adopt.md",
			"user-guide/quickstart.md",
		},
	})
}

// gateFlag collects repeated --gate values, each optionally ";;"-separated so
// a single flag can carry a full command list non-interactively.
type gateFlag []string

func (g *gateFlag) String() string { return strings.Join(*g, ";;") }
func (g *gateFlag) Set(v string) error {
	for _, part := range strings.Split(v, ";;") {
		if part = strings.TrimSpace(part); part != "" {
			*g = append(*g, part)
		}
	}
	return nil
}

// cmdAdopt drives the adoption wizard: detect -> plan -> confirm -> execute ->
// verify (docs/designs/2026-07-adopt.md). All prompts go to stderr and all
// machine output to stdout; in --json mode the streamed progress lines move to
// stderr so stdout stays pure JSON.
func cmdAdopt(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("adopt", stderr)
	yes := fs.Bool("yes", false, "non-interactive: accept unambiguous derivations, fail closed on ambiguity")
	dryRun := fs.Bool("dry-run", false, "detect + print the adoption plan, write nothing")
	asJSON := fs.Bool("json", false, "emit the plan (and results) as JSON on stdout; implies non-interactive")
	accountFlag := fs.String("account", "", "account profile (with --identity; overrides discovery)")
	identityFlag := fs.String("identity", "", "login email that must match at dispatch (with --account)")
	configDir := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR for non-personal accounts")
	id := fs.String("id", "", "project slug (default: repo dir name slugified)")
	branch := fs.String("branch", "", "default branch (default: detected)")
	var gates gateFlag
	fs.Var(&gates, "gate", `gate command (repeatable, or one ";;"-separated list); overrides inference`)
	forgeFlag := fs.String("forge", "", "forge provider github|gitlab (overrides inference)")
	remoteFlag := fs.String("remote", "", "beads sync remote URL (overrides the derived origin)")
	noRemote := fs.Bool("no-remote", false, "force a local-only beads init (no sync remote)")
	noPosture := fs.Bool("no-posture", false, "skip the posture profile offer")
	noCommit := fs.Bool("no-commit", false, "skip the adoption commit offer")
	force := fs.Bool("force", false, "override an .envrc account-disagreement refusal")
	setUsage(fs, stdout, "wizard: take an existing repo to a green `koryph validate` in one run",
		"[<root>] [--yes] [--dry-run] [--json] [flags]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	rootArg := "."
	if len(pos) > 0 {
		rootArg = pos[0]
	}
	root, err := filepath.Abs(rootArg)
	if err != nil {
		return fail(stderr, err)
	}

	ctx := context.Background()
	snap, err := adopt.Detect(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	if *id != "" {
		snap.ProjectID = *id
	}
	if flagPassed(fs, "branch") && *branch != "" {
		snap.Inventory.DefaultBranch = *branch
	}

	plan := adopt.BuildPlan(snap)
	interactive := isTerminal(os.Stdin) && !*yes && !*asJSON

	// out carries the streamed progress lines; stdout stays machine-clean in
	// --json mode.
	out := stdout
	if *asJSON {
		out = stderr
	}

	if *dryRun {
		if *asJSON {
			return jsonPlanExit(stdout, stderr, snap, plan)
		}
		adopt.RenderPlan(stdout, snap.ProjectID, snap.Root, plan)
		return 0
	}

	// --- confirm: value resolutions (design §3.3, scope 3) -------------------
	in := bufio.NewReader(os.Stdin)

	var acct adopt.AccountChoice
	if stepState(plan, adopt.StepRegister) == adopt.StateNeeded {
		if interactive && *accountFlag == "" && *identityFlag == "" {
			acct, err = promptAccount(in, stderr, snap)
		} else {
			acct, err = adopt.ResolveAccountNonInteractive(snap.AccountCandidates, *accountFlag, *identityFlag, *configDir)
		}
		if err != nil {
			return fail(stderr, err)
		}
	}

	var gate []string
	var forgeName string
	if stepState(plan, adopt.StepConfig) == adopt.StateNeeded {
		gate, err = adopt.ResolveGateNonInteractive(snap.GateProposals, gates)
		if err == nil {
			forgeName, err = adopt.ResolveForgeNonInteractive(snap.ForgeProposal, *forgeFlag, snap.Inventory.Remote)
		}
		if err != nil {
			if !interactive {
				return fail(stderr, err)
			}
			fmt.Fprintf(stderr, "koryph: %v\n", err)
			return 1
		}
		if interactive && !confirmConfig(in, stderr, snap, gate, forgeName) {
			fmt.Fprintln(stderr, "koryph: adoption cancelled — re-run with --gate/--forge to override the proposals")
			return 0
		}
	}

	// --- confirm: repo-scope consolidated consent (design §3.3, scope 1) -----
	adopt.RenderPlan(out, snap.ProjectID, snap.Root, plan)
	if interactive {
		if !promptYN(in, stderr, "Proceed with this plan?", false) {
			fmt.Fprintln(stderr, "koryph: adoption cancelled — nothing was written")
			return 0
		}
	}

	// --- execute (design §3.4) ------------------------------------------------
	blocked := 0

	// 1. deps — per-item system-scope consent; a decline/failure blocks the
	// tool but never aborts the wizard. A missing bd on a flake-managed repo
	// is offered the repo's own flake first (design §4.1) — the system route
	// only runs when the flake route is absent, declined, or fails.
	for _, name := range []string{"claude", "bd", "gh"} {
		ts, ok := snap.Tools[name]
		if !ok || (ts.Found && ts.VersionOK) {
			continue
		}
		if name == "bd" && !ts.Found && snap.FlakeNixPresent &&
			installBDViaFlake(ctx, in, out, stderr, root, interactive, *yes || *asJSON) {
			continue
		}
		if !installTool(ctx, in, out, stderr, ts, interactive, *yes || *asJSON) {
			blocked++
		}
	}

	// 2. home
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	stream(out, "ok", "home", "~/.koryph initialized")

	// 3. beads — before assets, so the settings merge dedupes bd's prime hook.
	// Re-probed here (not read from the snapshot) because step 1 may have just
	// installed bd.
	if execx.LookPath(beads.ResolveBin()) {
		res, actions, berr := adopt.ExecuteBeads(ctx, root, adopt.BeadsOpts{
			Prefix:         snap.ProjectID,
			RemoteURL:      snap.Inventory.Remote,
			RemoteOverride: *remoteFlag,
			NoRemote:       *noRemote,
		})
		if berr != nil {
			return fail(stderr, berr)
		}
		reportBeads(out, res, actions)
	} else {
		if snap.FlakeNixPresent {
			stream(out, "block", "beads", "bd is not on PATH — if the repo flake provides it, re-run `koryph adopt` inside the dev shell (`direnv allow` or `nix develop`); otherwise install bd and re-run")
		} else {
			stream(out, "block", "beads", "bd is not installed — install it and re-run `koryph adopt`")
		}
		blocked++
	}

	// 4. register + config
	rec := snap.ExistingRecord
	if stepState(plan, adopt.StepRegister) == adopt.StateNeeded || stepState(plan, adopt.StepConfig) == adopt.StateNeeded {
		var cfgErr error
		rec, _, cfgErr = adopt.RegisterAndConfigure(ctx, store, snap, acct, gate, forgeName, snap.AreaMap, *force)
		if cfgErr != nil {
			return fail(stderr, cfgErr)
		}
		if snap.ExistingRecord == nil {
			stream(out, "ok", "register", fmt.Sprintf("registered %s (account %s <%s>, %s)", rec.ProjectID, acct.Profile, acct.Identity, acct.Provenance))
		} else {
			stream(out, "skip", "register", "already registered")
		}
		if stepState(plan, adopt.StepConfig) == adopt.StateNeeded {
			stream(out, "ok", "config", configSummary(gate, forgeName, snap.AreaMap))
		} else {
			stream(out, "skip", "config", "existing config kept")
		}
	} else {
		stream(out, "skip", "register", "already registered")
		stream(out, "skip", "config", "existing config kept")
	}

	// 5. assets
	adopt.InstallAssets(stderr, root)
	stream(out, "ok", "assets", "AGENTS.md, agents, commands, hooks + settings.json ensured")

	// 6. offers — guidance only for signing; posture reuses the project-add
	// offer (which itself degrades to guidance in non-interactive shells).
	if stepState(plan, adopt.StepSigning) == adopt.StateOffer {
		stream(out, "offer", "signing", "enable later: `koryph signing keygen` (no-vault) or `koryph signing setup` (vault-backed)")
	}
	if !*noPosture && stepState(plan, adopt.StepPosture) == adopt.StateOffer {
		if code := cmdProjectAddPosture(root, "", out, stderr); code != 0 {
			fmt.Fprintf(stderr, "koryph: posture offer failed (run `koryph posture apply` manually): exit %d\n", code)
		}
	}

	// 7. commit
	if *noCommit {
		stream(out, "skip", "commit", "--no-commit")
	} else if code := offerCommit(ctx, in, out, stderr, root, interactive); code != 0 {
		return code
	}

	// 8. verify
	projectID := snap.ProjectID
	if rec != nil {
		projectID = rec.ProjectID
	}
	v, verr := onboard.Validate(ctx, store, projectID, out)
	if verr != nil {
		return fail(stderr, verr)
	}
	if !v.OK {
		fmt.Fprintln(out, "FAILED")
		if *asJSON {
			return jsonPlanExit(stdout, stderr, snap, replan(ctx, snap))
		}
		return engine.ExitFatal
	}
	promoteOnGreen(ctx, out, stderr, store, projectID)

	if *asJSON {
		return jsonPlanExit(stdout, stderr, snap, replan(ctx, snap))
	}
	fmt.Fprintf(out, "\nadopted: %s is ready — next: /koryph-plan a design (or /koryph-import existing TODOs), then\n  koryph run --project %s --once --dry-run\n", projectID, projectID)
	if blocked > 0 {
		fmt.Fprintf(stderr, "koryph: %d step(s) remain blocked — see the lines above\n", blocked)
		return 1
	}
	return 0
}

// replan re-detects and rebuilds the plan so --json output reflects the
// post-execute state rather than the pre-execute one.
func replan(ctx context.Context, snap *adopt.Snapshot) []adopt.Step {
	fresh, err := adopt.Detect(ctx, snap.Root)
	if err != nil {
		return nil
	}
	fresh.ProjectID = snap.ProjectID
	return adopt.BuildPlan(fresh)
}

// jsonPlanExit emits the machine-readable plan on stdout.
func jsonPlanExit(stdout, stderr io.Writer, snap *adopt.Snapshot, plan []adopt.Step) int {
	if err := printJSON(stdout, struct {
		Root      string       `json:"root"`
		ProjectID string       `json:"project_id"`
		Steps     []adopt.Step `json:"steps"`
	}{snap.Root, snap.ProjectID, plan}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// stepState returns the state of the first plan step with the given id
// ("" when absent). The tools category may repeat; every other id is unique.
func stepState(plan []adopt.Step, id adopt.StepID) adopt.StepState {
	for _, s := range plan {
		if s.ID == id {
			return s.State
		}
	}
	return ""
}

// stream prints one execute-phase progress line in validate's check style.
func stream(w io.Writer, state, title, detail string) {
	fmt.Fprintf(w, "%-5s %s — %s\n", state, title, detail)
}

// promptYN asks a y/N (or Y/n) question on stderr and reads one line.
func promptYN(in *bufio.Reader, stderr io.Writer, question string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	fmt.Fprintf(stderr, "%s %s ", question, suffix)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "":
		return defaultYes
	default:
		return false
	}
}

// promptAccount interactively picks the account/identity: one verified
// candidate needs a single keystroke; several offer a numbered choice; none
// fails with auth guidance (the same message the non-interactive resolver
// gives, since typing an unverifiable identity would only fail at dispatch).
func promptAccount(in *bufio.Reader, stderr io.Writer, snap *adopt.Snapshot) (adopt.AccountChoice, error) {
	var verified []int
	for i, c := range snap.AccountCandidates {
		if c.Verified {
			verified = append(verified, i)
		}
	}
	switch len(verified) {
	case 0:
		return adopt.ResolveAccountNonInteractive(snap.AccountCandidates, "", "", "")
	case 1:
		c := snap.AccountCandidates[verified[0]]
		if promptYN(in, stderr, fmt.Sprintf("Use account %s <%s> (%s)?", c.Profile.Name, c.Identity, c.Provenance), true) {
			return adopt.AccountChoice{Profile: c.Profile.Name, ConfigDir: c.Profile.ConfigDir, Identity: c.Identity, Provenance: c.Provenance}, nil
		}
		return adopt.AccountChoice{}, fmt.Errorf("adopt: account declined — re-run with --account/--identity")
	default:
		fmt.Fprintln(stderr, "Several verified accounts were found:")
		for n, idx := range verified {
			c := snap.AccountCandidates[idx]
			fmt.Fprintf(stderr, "  %d. %s <%s> (%s)\n", n+1, c.Profile.Name, c.Identity, c.Provenance)
		}
		fmt.Fprint(stderr, "Choose [1-", len(verified), "]: ")
		line, _ := in.ReadString('\n')
		var n int
		if _, serr := fmt.Sscanf(strings.TrimSpace(line), "%d", &n); serr != nil || n < 1 || n > len(verified) {
			return adopt.AccountChoice{}, fmt.Errorf("adopt: no account chosen — re-run with --account/--identity")
		}
		c := snap.AccountCandidates[verified[n-1]]
		return adopt.AccountChoice{Profile: c.Profile.Name, ConfigDir: c.Profile.ConfigDir, Identity: c.Identity, Provenance: c.Provenance}, nil
	}
}

// confirmConfig shows the derived gate/forge/area_map with provenance and asks
// for one bundled confirmation (design §10: "forge/area accepted together").
func confirmConfig(in *bufio.Reader, stderr io.Writer, snap *adopt.Snapshot, gate []string, forgeName string) bool {
	fmt.Fprintln(stderr, "Derived project config:")
	for i, g := range gate {
		prov := ""
		if i < len(snap.GateProposals) && snap.GateProposals[i].Value == g {
			prov = " (" + snap.GateProposals[i].Provenance + ")"
		}
		fmt.Fprintf(stderr, "  gate: %s%s\n", g, prov)
	}
	if forgeName != "" {
		fmt.Fprintf(stderr, "  forge: %s (%s)\n", forgeName, snap.ForgeProposal.Provenance)
	}
	if len(snap.AreaMap) > 0 {
		fmt.Fprintf(stderr, "  area_map: %d starter area(s) — %s\n", len(snap.AreaMap), snap.AreaMapProvenance)
	}
	return promptYN(in, stderr, "Accept this config? (the gate is your merge safety net)", true)
}

// installTool runs one tool's consented install plan. Returns false when the
// tool remains missing (declined, manual-only, sudo-in-yes-mode, or a failed
// install/verify) — the step is blocked, not fatal.
func installTool(ctx context.Context, in *bufio.Reader, out, stderr io.Writer, ts adopt.ToolStatus, interactive, assumeYes bool) bool {
	if ts.Found && !ts.VersionOK {
		stream(out, "block", "tools", ts.Name+" too old — "+ts.Remediation)
		return false
	}
	plan := ts.Plan
	if plan == nil || plan.Route == sysdeps.ManagerManual {
		detail := ts.Name + " not found"
		if plan != nil {
			detail += " — " + plan.Manual
		}
		stream(out, "block", "tools", detail)
		return false
	}
	argvText := strings.Join(plan.Argv, " ")
	switch {
	case interactive:
		note := ""
		if plan.NeedsSudo {
			note = " (requires sudo)"
		}
		if !promptYN(in, stderr, fmt.Sprintf("Install %s by running `%s`%s?", ts.Name, argvText, note), false) {
			stream(out, "block", "tools", ts.Name+" declined — install manually: "+argvText)
			return false
		}
	case plan.NeedsSudo:
		// --yes never elevates on the operator's behalf.
		stream(out, "block", "tools", ts.Name+" needs sudo — run manually: "+argvText)
		return false
	case !assumeYes:
		stream(out, "block", "tools", ts.Name+" not installed (no consent) — run: "+argvText)
		return false
	}
	res, rerr := execx.Run(ctx, execx.Cmd{Name: plan.Argv[0], Args: plan.Argv[1:]})
	if rerr != nil || res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if rerr != nil {
			detail = rerr.Error()
		}
		stream(out, "block", "tools", fmt.Sprintf("%s install failed (%s): %s", ts.Name, argvText, detail))
		return false
	}
	if len(plan.Verify) > 0 {
		vres, verr := execx.Run(ctx, execx.Cmd{Name: plan.Verify[0], Args: plan.Verify[1:]})
		if verr != nil || vres.ExitCode != 0 {
			stream(out, "block", "tools", ts.Name+" installed but `"+strings.Join(plan.Verify, " ")+"` failed — check your PATH")
			return false
		}
	}
	stream(out, "ok", "tools", ts.Name+" installed via "+string(plan.Route))
	return true
}

// installBDViaFlake offers the repo's own flake.nix as bd's install route
// (design §4.1): the proposed edit is shown as a diff before writing, `nix
// flake lock` runs after, and the plan's Verify argv proves bd is reachable.
// Returns true only when bd is verifiably available afterwards; any decline,
// planning error, or failure returns false so the caller falls back to the
// system route.
func installBDViaFlake(ctx context.Context, in *bufio.Reader, out, stderr io.Writer, root string, interactive, assumeYes bool) bool {
	if !execx.LookPath("nix") {
		return false
	}
	edit, err := sysdeps.PlanFlakeBeads(root)
	if err != nil || edit == nil {
		if err != nil {
			stream(out, "warn", "tools", "flake route unavailable ("+err.Error()+") — falling back to a system install of bd")
		}
		return false
	}
	if !edit.AlreadyIntegrated {
		if interactive {
			fmt.Fprintf(stderr, "This repo uses a nix flake. Proposed flake.nix change to provide bd:\n%s\n", edit.Diff)
			if !promptYN(in, stderr, "Apply this flake.nix edit (then `nix flake lock`)?", true) {
				return false
			}
		} else if !assumeYes {
			return false
		}
		if aerr := sysdeps.ApplyFlakeEdit(ctx, root, edit); aerr != nil {
			stream(out, "warn", "tools", "flake edit failed ("+aerr.Error()+") — falling back to a system install of bd")
			return false
		}
	}
	if len(edit.Verify) > 0 {
		vres, verr := execx.Run(ctx, execx.Cmd{Dir: root, Name: edit.Verify[0], Args: edit.Verify[1:]})
		if verr != nil || vres.ExitCode != 0 {
			stream(out, "warn", "tools", "flake provides beads but `"+strings.Join(edit.Verify, " ")+"` failed — falling back to a system install of bd")
			return false
		}
	}
	stream(out, "ok", "tools", "bd provided via the repo flake (flake.nix + nix flake lock)")
	return true
}

// reportBeads streams the beads step's outcome.
func reportBeads(out io.Writer, res beads.EnsureResult, actions []beads.HardenAction) {
	switch {
	case res.Initialized:
		stream(out, "ok", "beads", "initialized ("+strings.Join(res.InitArgv, " ")+")")
		if res.RemoteNote != "" {
			stream(out, "warn", "beads", res.RemoteNote)
		}
	case res.DoctorRan && !res.DoctorOK:
		stream(out, "warn", "beads", "existing DB — `bd doctor` reported issues:\n"+indent(res.DoctorOutput))
	default:
		stream(out, "ok", "beads", "existing DB healthy (snapshot: "+res.SnapshotPath+")")
	}
	if !res.VersionOK && res.Remediation != "" {
		stream(out, "warn", "beads", res.Remediation)
	}
	for _, a := range actions {
		if a.Applied {
			stream(out, "ok", "beads", a.Name+": "+a.Detail)
		} else {
			stream(out, "warn", "beads", a.Name+": "+a.Detail)
		}
	}
}

// offerCommit runs the adoption-commit step (design §3.4 step 7).
func offerCommit(ctx context.Context, in *bufio.Reader, out, stderr io.Writer, root string, interactive bool) int {
	dirty, err := adopt.DirtyAdoptionPaths(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	if len(dirty) == 0 {
		stream(out, "skip", "commit", "nothing to commit")
		return 0
	}
	if interactive {
		fmt.Fprintf(stderr, "The wizard wrote %d file(s):\n  %s\n", len(dirty), strings.Join(dirty, "\n  "))
		if !promptYN(in, stderr, `Commit them now as "chore: adopt koryph"?`, true) {
			stream(out, "skip", "commit", fmt.Sprintf("declined — %d file(s) left uncommitted: %s", len(dirty), strings.Join(dirty, ", ")))
			return 0
		}
	}
	committed, files, cerr := adopt.CommitAdoption(ctx, root, "")
	if cerr != nil {
		return fail(stderr, cerr)
	}
	if committed {
		stream(out, "ok", "commit", fmt.Sprintf("committed %d file(s): chore: adopt koryph", len(files)))
	} else {
		stream(out, "skip", "commit", "nothing to commit")
	}
	return 0
}

// promoteOnGreen mirrors cmdValidate's registered->migrated promotion (the
// migrated->validated rung needs a canary run and stays with `koryph
// validate`).
func promoteOnGreen(ctx context.Context, out, stderr io.Writer, store *registry.Store, projectID string) {
	rec, err := store.Get(projectID)
	if err != nil || rec.MigrationStatus != registry.StatusRegistered {
		return
	}
	rec.MigrationStatus = registry.StatusMigrated
	if serr := store.Save(ctx, rec); serr != nil {
		fmt.Fprintln(stderr, "koryph: warning: could not promote migration status:", serr)
		return
	}
	fmt.Fprintln(out, "promoted migration_status: registered -> migrated")
}

// configSummary renders the config execute line.
func configSummary(gate []string, forgeName string, areaMap map[string][]string) string {
	parts := []string{fmt.Sprintf("gate: %s", strings.Join(gate, ", "))}
	if forgeName != "" {
		parts = append(parts, "forge: "+forgeName)
	}
	if len(areaMap) > 0 {
		parts = append(parts, fmt.Sprintf("area_map: %d area(s)", len(areaMap)))
	}
	return strings.Join(parts, "; ")
}

// indent prefixes every line of s with six spaces (aligning under the stream
// detail column).
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}
