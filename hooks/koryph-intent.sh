#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
#
# UserPromptSubmit hook: intent → beads routing (design:
# docs/designs/2026-07-intent-routing.md). An adopted repo's CLAUDE.md
# predates koryph or doesn't exist at all, so nothing in the project's own
# instruction files tells a session that implementation work must become
# beads. This hook closes that gap at runtime: when the operator's prompt
# reads like a description of something to BUILD/CHANGE/FIX (rather than a
# question or an explicit command), it injects a small routing rubric
# pointing at the planning commands (/koryph-design, /koryph-plan,
# /koryph-import, /koryph-issue) that decompose intent into
# dispatch-shaped, footprint-labeled beads.
#
# Invariants:
#   I1 — fail-open: always exit 0; a broken hook must never block a prompt.
#        Missing jq, unparsable stdin, or any internal error → no output.
#   I2 — never fires inside a koryph dispatch (KORYPH_PHASE_ID or
#        KORYPH_SPAWN_KIND set): a dispatched agent's work already IS a
#        bead; the rubric would invite recursive planning.
#   I3 — byte-frugal (docs/designs/2026-07-token-economy.md): the rubric is
#        under 1KB and only injected when the heuristic matches; questions,
#        slash/shell/memory-prefixed prompts, and short prompts inject
#        nothing.
#   I4 — advisory, not a gate: never blocks or rewrites the prompt; the
#        rubric itself tells the model to ignore the map for questions and
#        trivial direct edits, so an over-trigger is harmless.
set -uo pipefail

# I2: koryph-dispatched and secondary-spawn sessions never get the rubric.
[[ -z "${KORYPH_PHASE_ID:-}" && -z "${KORYPH_SPAWN_KIND:-}" ]] || exit 0

# I1: no jq → no detection; fail open silently.
command -v jq >/dev/null 2>&1 || exit 0

prompt="$(jq -r '.prompt // empty' 2>/dev/null)" || exit 0

# Slash commands, shell (!) and memory (#) prefixes already route
# explicitly; a sub-24-char prompt is too small to be a work description.
case "${prompt}" in "/"* | "!"* | "#"*) exit 0 ;; esac
[[ "${#prompt}" -ge 24 ]] || exit 0

lower="$(printf '%s' "${prompt}" | tr '[:upper:]' '[:lower:]')"

# Question-opener suppressor: developer questions constantly contain the
# intent vocabulary as NOUNS ("why is the build slow", "what closed the
# bug") — a prompt that opens interrogatively is a question, and questions
# must stay silent (I3). A mis-suppressed ask still routes via the model's
# own judgment and AGENTS.md (I4), so this errs toward silence.
question='^(why|what|whats|what.s|how|hows|how.s|where|wheres|where.s|when|which|who|whose|is|are|was|were|does|do|did|explain|describe|tell me|show me|summari[sz]e|walk me through)([^a-z]|$)'
[[ "${lower}" =~ ${question} ]] && exit 0

# Work-intent shapes: imperative build/change/fix verbs and want-phrases.
# Morphology guard: the ([^a-z]|$) tail keeps "created"/"updated"/"builds"
# from matching — only the bare verb/noun forms count.
intent='(^|[^a-z])(build|implement|add|create|make|fix|repair|refactor|redesign|rework|rewrite|migrate|integrate|support|extend|improve|optimi[sz]e|automate|upgrade|replace|remove|rename|port|modify|change|update|broken|bug|feature|enhancement)([^a-z]|$)'
want='(i want|we want|i need|we need|i.d like|we.d like|should (be able|support|have|handle)|let.s (build|add|make|create))'
[[ "${lower}" =~ ${intent} || "${lower}" =~ ${want} ]] || exit 0

rubric='This koryph-managed project tracks implementation work as beads (bd issues) so the wave engine can build it under merge discipline. If this prompt describes something to build, change, or fix, do NOT implement it ad hoc — map it to beads first: /koryph-design <ask> for feature-sized or multi-part work (writes a repo-grounded design doc, then decomposes it); /koryph-plan <path> when a design doc already exists; /koryph-import [path] for existing TODO/roadmap markdown; /koryph-issue <desc> for one small self-contained fix or chore. These commands compute the footprint (area:*/fp:*) and resource (res:*) labels the parallel scheduler needs. If the prompt is a question, just answer it; a trivial edit the operator asked you to make directly needs no bead.'

jq -cn --arg r "${rubric}" '{hookSpecificOutput:{hookEventName:"UserPromptSubmit",additionalContext:$r}}' 2>/dev/null || exit 0
exit 0
