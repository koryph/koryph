// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

// resources.go — per-kind external-resource probe diffing (koryph-4ql.8,
// docs/designs/2026-07-resource-governor.md L7 "Per-kind probe (opt-in)").
//
// A resource kind's machine config (governor.json's top-level "resources"
// section, govern.ResourceKind.Probe) may carry an operator-authored shell
// command that lists a kind's live instance names (e.g. `kind get
// clusters`). `koryph doctor` and the engine health patrol both diff that
// output against the `<kind>-<bead-id>` naming convention the RESOURCES
// prompt block asks agents to use (design L6) and the governor's live
// leases: an instance matching the convention whose bead-id suffix has no
// live lease is a suspected leak — a freed lease is not a torn-down instance
// (design §7 "Leaked instances").
//
// DiffResourceProbe is the shared diff primitive (koryph-4ql.8 AC: "shared
// diff logic ... without an import cycle"). internal/doctor already imports
// internal/govern (the checkZombieLeases precedent) and nothing internal/
// doctor depends on imports internal/engine, so the shared home is here:
// internal/engine's health patrol imports internal/doctor for this helper,
// never the reverse. The two surfaces otherwise stay independent — each
// reads governor.json/the slots dir its own way (doctor via the injectable
// Options.Home, the patrol via paths.GovernorConfig/paths.SlotsDir) and
// calls the same diff function with what it found.
//
// Probes run ONLY from doctor/the patrol, never the admission path (I7), and
// are fail-soft (I6): a probe error (non-zero exit, or a spawn/timeout
// failure) surfaces as a single skipped note, never a leak finding — an
// unconfigured or flaky probe binary must not manufacture false leaks.
// Report-only: nothing here ever signals a process or deletes a lease file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/govern"
)

// checkNameResourceProbe is the doctor check label for per-kind probe-diffing
// findings. The engine health patrol defines its own const with the
// identical literal value (patrolCheckEpicVal/checkNameEpicValidations
// precedent) so both surfaces report under the same check name without a
// cross-package const dependency.
const checkNameResourceProbe = "resource-probe"

// resourceProbeTimeout bounds one probe command's wall-clock time. Probes are
// operator-authored shell (same trust model as the project gate) but run
// unattended on a patrol/doctor cadence, so a hung probe must not wedge the
// check indefinitely.
const resourceProbeTimeout = 15 * time.Second

// ResourceLeakFinding is one suspected leaked external-resource instance:
// probe output matched the `<kind>-<bead-id>` naming convention (design L6)
// but no live lease exists for that bead-id, of this kind, in the governor.
type ResourceLeakFinding struct {
	Kind     string
	Instance string
	BeadID   string
}

// Message renders the operator-facing narrative, echoing the instance name in
// a suggested manual teardown command (design L7: "leak finding with a
// suggested manual teardown command echoing the instance name"). Shared
// verbatim by `koryph doctor` and the engine health patrol.
func (f ResourceLeakFinding) Message() string {
	return fmt.Sprintf(
		"kind %s: instance %q (bead %s) has no live lease — suspected leak; verify and tear down manually, e.g. your %s teardown command against %q",
		f.Kind, f.Instance, f.BeadID, f.Kind, f.Instance)
}

// RunProbeShell is the production probe runner: `sh -c <cmd>`, bounded by
// resourceProbeTimeout. A non-zero exit and a spawn/timeout failure both
// return a non-nil error — DiffResourceProbe's caller treats either as a
// fail-soft skip, never a finding.
func RunProbeShell(ctx context.Context, cmd string) (string, error) {
	res, err := execx.Run(ctx, execx.Cmd{Name: "sh", Args: []string{"-c", cmd}, Timeout: resourceProbeTimeout})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("probe exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// DiffResourceProbe runs kind's probe command and returns suspected leaked
// instances: probe-output lines matching the `<kind>-` naming-convention
// prefix (design L6) whose bead-id suffix is absent from liveBeadIDs — the
// set of bead ids currently holding a live lease for this kind (see
// LiveResourceHolders for doctor's/the patrol's shared raw-lease-file
// source).
//
// probeCmd == "" (kind has no configured probe — opt-in, design L7) is a
// no-op: (nil, nil). A probe run error degrades to (nil, err) — fail-soft
// (I6): the caller surfaces one "skipped" note instead of a leak finding.
// run defaults to RunProbeShell when nil.
func DiffResourceProbe(ctx context.Context, kind, probeCmd string, liveBeadIDs map[string]bool, run func(context.Context, string) (string, error)) ([]ResourceLeakFinding, error) {
	if probeCmd == "" {
		return nil, nil
	}
	if run == nil {
		run = RunProbeShell
	}
	out, err := run(ctx, probeCmd)
	if err != nil {
		return nil, err
	}

	prefix := kind + "-"
	seen := map[string]bool{}
	var findings []ResourceLeakFinding
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		beadID := strings.TrimPrefix(name, prefix)
		if beadID == name {
			continue // no "<kind>-" prefix: does not match the naming convention, ignore
		}
		if liveBeadIDs[beadID] {
			continue // a live lease of this kind accounts for the instance
		}
		findings = append(findings, ResourceLeakFinding{Kind: kind, Instance: name, BeadID: beadID})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Instance < findings[j].Instance })
	return findings, nil
}

// LiveResourceHolders scans every lease file under slotsDir (across every
// provider pool — machine resources are cross-pool, design L2) and groups
// the LIVE leases' bead ids by the resource kinds they hold. "Live" mirrors
// checkZombieLeases' precedent exactly: the agent pid is alive, OR the agent
// pid is dead/never-launched but the owning engine pid is still alive (the
// normal shape once an agent exits into a post-build review/rebase/gate/
// merge stage, koryph-p42). A lease satisfying neither is a zombie candidate
// for checkZombieLeases/patrolCheckZombieLeases and deliberately does NOT
// count as "live" here — an instance whose only lease is already a zombie is
// exactly the case DiffResourceProbe should flag as a suspected leak.
//
// alive is injectable (opts.alive in doctor, dispatch.Alive in the patrol).
func LiveResourceHolders(slotsDir string, alive func(pid int) bool) (map[string]map[string]bool, error) {
	entries, err := os.ReadDir(slotsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]map[string]bool{}, nil
		}
		return nil, err
	}

	out := map[string]map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(slotsDir, e.Name()))
		var l govern.Lease
		if rerr != nil || json.Unmarshal(data, &l) != nil || l.Project == "" {
			continue
		}

		probePID := l.PID
		if probePID <= 0 {
			probePID = l.EnginePID
		}
		live := probePID > 0 && alive(probePID)
		if !live && l.EnginePID > 0 && alive(l.EnginePID) {
			live = true
		}
		if !live {
			continue
		}

		for _, kind := range l.Resources {
			if out[kind] == nil {
				out[kind] = map[string]bool{}
			}
			out[kind][l.Bead] = true
		}
	}
	return out, nil
}

// LoadResourcesConfig reads governor.json at path and returns its top-level
// "resources" section (nil on any read/parse failure, or when the section is
// absent) — fail-open per I6: a missing or corrupt governor.json degrades to
// "no probes configured", never an error finding from callers.
func LoadResourcesConfig(path string) *govern.ResourcesConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f govern.File
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Resources
}

// checkResourceProbes is the doctor check for koryph-4ql.8's probe-diffing
// half (design L7): for every configured resource kind carrying a non-empty
// probe command, run it and diff its output against the governor's live
// leases via DiffResourceProbe, reporting one WARN Finding per suspected
// leaked instance. Kinds with no configured probe are skipped silently
// (opt-in). A probe failure degrades to a single OK "skipped" note (I6
// fail-open) rather than a finding.
func checkResourceProbes(opts Options) []Finding {
	rc := LoadResourcesConfig(filepath.Join(opts.home(), "governor.json"))
	if rc == nil || len(rc.Kinds) == 0 {
		return []Finding{{Check: checkNameResourceProbe, Level: LevelOK, Message: "no resource kinds configured"}}
	}

	kinds := make([]string, 0, len(rc.Kinds))
	for k, spec := range rc.Kinds {
		if spec.Probe != "" {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return []Finding{{Check: checkNameResourceProbe, Level: LevelOK, Message: "no per-kind probes configured"}}
	}
	sort.Strings(kinds)

	holders, err := LiveResourceHolders(filepath.Join(opts.home(), "slots"), opts.alive)
	if err != nil {
		return []Finding{{Check: checkNameResourceProbe, Level: LevelWarn,
			Message: fmt.Sprintf("read slots dir: %v", err)}}
	}

	ctx := context.Background()
	run := opts.runProbe()
	var findings []Finding
	for _, kind := range kinds {
		leaks, perr := DiffResourceProbe(ctx, kind, rc.Kinds[kind].Probe, holders[kind], run)
		if perr != nil {
			findings = append(findings, Finding{Check: checkNameResourceProbe, Level: LevelOK,
				Message: fmt.Sprintf("kind %s: probe failed, skipped: %v", kind, perr)})
			continue
		}
		if len(leaks) == 0 {
			findings = append(findings, Finding{Check: checkNameResourceProbe, Level: LevelOK,
				Message: fmt.Sprintf("kind %s: probe ok, no orphaned instances", kind)})
			continue
		}
		for _, lk := range leaks {
			findings = append(findings, Finding{Check: checkNameResourceProbe, Level: LevelWarn, Message: lk.Message()})
		}
	}
	return findings
}
