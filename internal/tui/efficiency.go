// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// efficiency.go implements the Efficiency + Calibration tab for the koryph TUI
// cockpit (koryph-9af.4, design §2.4). It is the "self-hosting case study
// rendered live": operators can see exactly why concurrency is what it is,
// which footprint tokens are most contended, and whether the quota estimator
// is tracking reality.
//
// Layout (top→bottom):
//  1. Dispatch Rate — dispatched-per-day sparkline + achieved vs permitted
//     concurrency gauge.
//  2. Top Deferral Tokens — write-tokens most frequently held by active slots
//     (the coupling measurement: high counts mean serial bottlenecks).
//  3. Governor Pools — per-pool cap / AIMD / probe / settle / breaker state.
//     3b. Resources — per-kind external resource ledger (capacity, live
//     holders, reserved-vs-materialized MB, ramp state), koryph-4ql.1 L7 /
//     koryph-4ql.10 (design docs/designs/2026-07-resource-governor.md §4
//     "Cockpit snapshots"). Empty when nothing has ever declared/configured
//     a res:<kind> — old snapshots simply omit the section.
//  4. Quota Windows — 5-hour and weekly burn bars (ceiling from config; live
//     spend unavailable in the TUI path — marked with a hint).
//  5. Estimator Calibration — per-(tier:size) bucket n / bias / MAPE /
//     corrected-USD derived from koryph-6bl ErrorStats.
package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Efficiency",
		Order: 3,
		New:   func(theme Theme, _ bool) TabModel { return newEfficiencyModel(theme) },
	})
}

// efficiencyModel is the Bubble Tea model for the Efficiency tab.
type efficiencyModel struct {
	theme  Theme
	width  int
	height int
	snap   cockpit.Snapshot
}

// newEfficiencyModel creates an empty efficiency model.
func newEfficiencyModel(theme Theme) *efficiencyModel {
	return &efficiencyModel{
		theme:  theme,
		width:  80,
		height: 24,
	}
}

// Init implements TabModel.
func (m *efficiencyModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Efficiency tab has no text inputs.
func (m *efficiencyModel) IsCapturingInput() bool { return false }

// Update implements TabModel (no scroll in this tab for v1).
func (m *efficiencyModel) Update(_ tea.Msg) (TabModel, tea.Cmd) {
	return m, nil
}

// SetSnapshot implements TabModel.
func (m *efficiencyModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
}

// Resize implements TabModel.
func (m *efficiencyModel) Resize(w, h int) {
	m.width = w
	m.height = h
}

// View implements TabModel.
func (m *efficiencyModel) View() string {
	eff := m.snap.Efficiency

	var b strings.Builder
	b.WriteString(m.renderDispatchSection(eff))
	b.WriteRune('\n')
	b.WriteString(m.renderDeferralSection(eff))
	b.WriteRune('\n')
	b.WriteString(m.renderGovernorSection(eff))
	b.WriteRune('\n')
	b.WriteString(m.renderResourcesSection(m.snap.Governor))
	b.WriteRune('\n')
	b.WriteString(m.renderQuotaSection(eff))
	b.WriteRune('\n')
	b.WriteString(m.renderEstimatorSection(eff))
	b.WriteRune('\n')
	b.WriteString(m.renderTokenSection(eff))

	return b.String()
}

// --- section renderers -------------------------------------------------------

// renderDispatchSection renders the dispatch rate + concurrency gauge.
func (m *efficiencyModel) renderDispatchSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Dispatch Rate")

	// Sparkline (reuse cockpit.SparklineLen width).
	spk := ""
	if len(eff.DispatchSparkline) > 0 {
		spk = renderSparklineFromFloats(eff.DispatchSparkline)
	}

	// Compute total dispatched in window and today.
	total := 0.0
	today := 0.0
	for i, v := range eff.DispatchSparkline {
		total += v
		if i == len(eff.DispatchSparkline)-1 {
			today = v
		}
	}

	spkLine := fmt.Sprintf("  dispatched last %d days: %s  (%.0f total  %.0f today)",
		cockpit.SparklineLen, spk, total, today)

	// Concurrency gauge.
	achieved := eff.AchievedConcurrency
	permitted := eff.PermittedConcurrency
	if permitted == 0 {
		permitted = 8 // default
	}
	utilPct := 0.0
	if permitted > 0 {
		utilPct = float64(achieved) / float64(permitted) * 100
	}

	barW := 20
	filled := int(math.Round(float64(barW) * float64(achieved) / float64(permitted)))
	if filled > barW {
		filled = barW
	}
	bar := "[" + strings.Repeat("█", filled) + strings.Repeat("░", barW-filled) + "]"

	concStyle := lipgloss.NewStyle().Foreground(m.concurrencyColor(utilPct))
	concLine := fmt.Sprintf("  concurrency: %s %d/%d (%.0f%%)",
		concStyle.Render(bar), achieved, permitted, utilPct)

	return title + "\n" + spkLine + "\n" + concLine
}

// renderDeferralSection renders the top deferral tokens table.
func (m *efficiencyModel) renderDeferralSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Top Deferral Tokens (write-locks held by active slots)")

	if len(eff.TopDeferralTokens) == 0 {
		return title + "\n" + m.dimText("  no active slots with footprint data")
	}

	// Column widths: token (flexible) | held-by count.
	tokenW := m.width - 20
	if tokenW < 12 {
		tokenW = 12
	}
	header := m.tableHeader(tokenW, "Token", 10, "Slots Locked")

	var rows []string
	for _, dt := range eff.TopDeferralTokens {
		tok := truncate(dt.Token, tokenW)
		held := fmt.Sprintf("%d", dt.HeldBy)
		// Highlight high-contention tokens.
		if dt.HeldBy >= 3 {
			held = lipgloss.NewStyle().Foreground(m.theme.Warning).Render(held)
		} else if dt.HeldBy >= 2 {
			held = lipgloss.NewStyle().Foreground(m.theme.Accent).Render(held)
		}
		rows = append(rows, fmt.Sprintf("  %-*s  %s", tokenW, tok, held))
	}

	return title + "\n" + header + "\n" + strings.Join(rows, "\n")
}

// renderGovernorSection renders per-pool governor state.
func (m *efficiencyModel) renderGovernorSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Governor Pools")

	if len(eff.GovernorPools) == 0 {
		return title + "\n" + m.dimText("  governor state unavailable")
	}

	var lines []string
	for _, p := range eff.GovernorPools {
		// Build a concise one-liner per pool:
		//   <provider>  cap:<N>  dyn:<N>  leases:<N>  [AIMD]  [settling]  [breaker=X]  [probe: proj/bead]
		parts := []string{
			fmt.Sprintf("  %-14s", truncate(p.Provider, 14)),
			fmt.Sprintf("cap:%-3d", p.Cap),
			fmt.Sprintf("dyn:%-3d", p.Dynamic),
			fmt.Sprintf("leases:%-2d", p.Leases),
		}
		if p.Adaptive {
			parts = append(parts, lipgloss.NewStyle().Foreground(m.theme.Accent).Render("AIMD"))
		}
		if p.Settling {
			settle := "settling"
			if p.SettleUntil != "" {
				// Show seconds remaining.
				if secs := m.settleSecsRemaining(p.SettleUntil); secs > 0 {
					settle = fmt.Sprintf("settling(%ds)", secs)
				}
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(m.theme.Warning).Render(settle))
		}
		if p.BreakerState != "" {
			bStyle := m.breakerStyle(p.BreakerState)
			parts = append(parts, bStyle.Render("breaker="+p.BreakerState))
		}
		if p.ProbeProject != "" || p.ProbeBead != "" {
			probe := truncate(p.ProbeProject+"/"+p.ProbeBead, 24)
			parts = append(parts, m.dimText("probe:"+probe))
		}
		lines = append(lines, strings.Join(parts, "  "))
	}

	return title + "\n" + strings.Join(lines, "\n")
}

// renderResourcesSection renders the per-kind external resource ledger
// (koryph-4ql.1 L7, koryph-4ql.10): resolved capacity, live holders
// (project/bead, with a "ramping" annotation), and reserved-vs-materialized
// MB. One concise line per kind, mirroring renderGovernorSection's per-pool
// label style (a fixed-width name column then colour-safe "key:value" parts
// joined by "  ", rather than a fixed-width table — several rendered parts
// here carry ANSI colour codes, which would misalign a %-*s column). Empty
// when gov.Resources is nil — an old cockpit snapshot, an unavailable
// governor, or simply a project that has never declared/configured a
// res:<kind> (design §4 "Cockpit snapshots": old snapshots omit the
// section).
func (m *efficiencyModel) renderResourcesSection(gov cockpit.GovernorSnapshot) string {
	title := m.sectionTitle("Resources")

	if len(gov.Resources) == 0 {
		return title + "\n" + m.dimText("  no declared resource kinds")
	}

	var lines []string
	for _, rs := range gov.Resources {
		n := len(rs.Holders)
		capStr := fmt.Sprintf("cap:%d/%d", n, rs.Capacity)
		if rs.Capacity > 0 && n >= rs.Capacity {
			capStr = lipgloss.NewStyle().Foreground(m.theme.Warning).Render(capStr)
		}

		parts := []string{
			fmt.Sprintf("  %-16s", truncate(rs.Kind, 16)),
			capStr,
			fmt.Sprintf("reserved:%dMB", rs.ReservedMB),
			fmt.Sprintf("materialized:%dMB", rs.MaterializedMB),
		}

		holderParts := make([]string, 0, len(rs.Holders))
		for _, h := range rs.Holders {
			id := h.Bead
			if h.Project != "" {
				id = h.Project + "/" + h.Bead
			}
			if h.Ramping {
				id += "(ramping)"
			}
			holderParts = append(holderParts, id)
		}
		if len(holderParts) > 0 {
			parts = append(parts, m.dimText("held:"+truncate(strings.Join(holderParts, ", "), 40)))
		}

		lines = append(lines, strings.Join(parts, "  "))
	}

	return title + "\n" + strings.Join(lines, "\n")
}

// renderQuotaSection renders the quota window burn bars, one labeled block
// per AI provider/runtime with measurable usage. Different dispatched
// threads may run under different runtimes (ledger.Slot.Runtime), each
// billed against its own provider's rate limits — this loop is the display
// side of that: it renders exactly one block today ("claude", the only
// runtime that exists) and needs no further change once a second provider's
// quota measurement lands.
func (m *efficiencyModel) renderQuotaSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Quota Windows")

	if len(eff.ProviderQuotas) == 0 {
		return title + "\n" + m.dimText("  no data yet")
	}

	barW := 24
	var b strings.Builder
	for i, pq := range eff.ProviderQuotas {
		if i > 0 {
			b.WriteRune('\n')
		}
		label := pq.Runtime
		if label == "" {
			label = pq.Provider
		}
		if label == "" {
			label = "unknown provider"
		}
		fmt.Fprintf(&b, "  %s\n", lipgloss.NewStyle().Bold(true).Foreground(m.theme.Cyan).Render(label))

		if pq.Source == "uncalibrated" {
			b.WriteString(m.dimText("    uncalibrated — run: koryph quota calibrate") + "\n")
			continue
		}

		line5h := m.quotaBar("5-hour ", pq.Window5hCeiling, pq.Window5hSpent, pq.Window5hFrac, barW, pq.Source)
		lineWeekly := m.quotaBar("weekly ", pq.WeeklyCeiling, pq.WeeklySpent, pq.WeeklyFrac, barW, pq.Source)
		fmt.Fprintf(&b, "%s\n%s\n", line5h, lineWeekly)

		switch pq.Source {
		case "unavailable":
			b.WriteString(m.dimText("    live spend unavailable — run: koryph quota usage") + "\n")
		case "jsonl-scan":
			b.WriteString(m.dimText("    spend ≈ transcript scan (refreshes every minute)") + "\n")
		}
	}

	return title + "\n" + strings.TrimRight(b.String(), "\n")
}

// renderEstimatorSection renders the per-bucket estimator calibration table.
func (m *efficiencyModel) renderEstimatorSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Estimator Calibration (koryph-6bl)")

	if len(eff.EstimatorRows) == 0 {
		return title + "\n" + m.dimText("  no calibration data yet (dispatches accumulate it)")
	}

	// Columns: bucket | n | bias | MAPE | corrected | base
	bucketW := 16
	nW := 5
	biasW := 8
	mapeW := 8
	corrW := 10
	baseW := 10
	header := m.tableHeader(
		bucketW, "Bucket",
		nW, "N",
		biasW, "Bias",
		mapeW, "MAPE%",
		corrW, "Corrected",
		baseW, "Base",
	)

	var rows []string
	for _, row := range eff.EstimatorRows {
		biasStr := m.dimText("—")
		mapeStr := m.dimText("—")
		if row.N > 0 {
			biasStr = fmt.Sprintf("%.3f", row.Bias)
			mapeStr = fmt.Sprintf("%.1f", row.MAPE)
			// Colour bias: green near 1.0, amber if >1.2 or <0.8, red if >1.5 or <0.5.
			biasStr = lipgloss.NewStyle().Foreground(m.biasColor(row.Bias)).Render(biasStr)
		}
		corrStr := m.dimText("—")
		if row.Corrected > 0 {
			corrStr = fmt.Sprintf("$%.3f", row.Corrected)
		}
		baseStr := ""
		if row.Base > 0 {
			baseStr = fmt.Sprintf("$%.3f", row.Base)
		}

		nStr := fmt.Sprintf("%d", row.N)
		if row.N == 0 {
			nStr = m.dimText("0")
		}

		rows = append(rows, fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %-*s  %s",
			bucketW, row.Bucket,
			nW-2, nStr,
			biasW-2, biasStr,
			mapeW-2, mapeStr,
			corrW-2, corrStr,
			baseStr,
		))
	}

	return title + "\n" + header + "\n" + strings.Join(rows, "\n")
}

// --- rendering helpers -------------------------------------------------------

// sectionTitle renders a section header bar (shared style with burndown tab).
func (m *efficiencyModel) sectionTitle(title string) string {
	bar := "─ " + title + " " + strings.Repeat("─", m.width-len(title)-4)
	if len(bar) > m.width {
		bar = bar[:m.width]
	}
	return lipgloss.NewStyle().Foreground(m.theme.Accent).Render(bar)
}

// tableHeader renders a fixed-column-width header row.
func (m *efficiencyModel) tableHeader(pairs ...interface{}) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		w := pairs[i].(int)
		t := pairs[i+1].(string)
		col := fmt.Sprintf("%-*s", w, t)
		parts = append(parts, lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(col))
	}
	return "  " + strings.Join(parts, "  ")
}

// dimText returns s styled in the inactive/gray color.
func (m *efficiencyModel) dimText(s string) string {
	return lipgloss.NewStyle().Foreground(m.theme.Inactive).Render(s)
}

// concurrencyColor returns the color for a concurrency utilization percentage.
func (m *efficiencyModel) concurrencyColor(pct float64) lipgloss.Color {
	switch {
	case pct >= 90:
		return m.theme.Done // at cap — great utilisation
	case pct >= 50:
		return m.theme.Accent
	case pct > 0:
		return m.theme.Warning
	default:
		return m.theme.Inactive
	}
}

// breakerStyle returns a lipgloss style for the breaker state.
func (m *efficiencyModel) breakerStyle(state string) lipgloss.Style {
	switch state {
	case "open":
		return lipgloss.NewStyle().Foreground(m.theme.Error)
	case "half-open":
		return lipgloss.NewStyle().Foreground(m.theme.Warning)
	default:
		return lipgloss.NewStyle().Foreground(m.theme.Done)
	}
}

// biasColor returns the color for a bias value (1.0 = perfect).
func (m *efficiencyModel) biasColor(bias float64) lipgloss.Color {
	diff := math.Abs(bias - 1.0)
	switch {
	case diff >= 0.5:
		return m.theme.Error
	case diff >= 0.2:
		return m.theme.Warning
	default:
		return m.theme.Done
	}
}

// quotaBar renders a single quota window burn bar.
// frac < 0 means spend is not available (render ceiling + hint).
func (m *efficiencyModel) quotaBar(label string, ceiling, spent, frac float64, barW int, source string) string {
	if ceiling == 0 {
		return "  " + label + ": " + m.dimText("uncalibrated")
	}

	ceilStr := fmt.Sprintf("$%.2f", ceiling)
	if frac < 0 {
		// Live spend unavailable — show ceiling only.
		bar := strings.Repeat("░", barW)
		return fmt.Sprintf("  %s[%s] ceil %s (spend unavailable)", label, bar, ceilStr)
	}

	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(float64(barW) * frac))
	if filled > barW {
		filled = barW
	}
	barColor := m.theme.Done
	switch {
	case frac >= 0.95:
		barColor = m.theme.Error
	case frac >= 0.90:
		barColor = m.theme.Warning
	case frac >= 0.80:
		barColor = m.theme.Yellow
	}

	filledBar := lipgloss.NewStyle().Foreground(barColor).Render(strings.Repeat("█", filled))
	emptyBar := strings.Repeat("░", barW-filled)
	pctStr := fmt.Sprintf("%.0f%%", frac*100)

	_ = source // source shown in the parent section
	return fmt.Sprintf("  %s[%s%s] $%.2f/%s (%s)",
		label, filledBar, emptyBar, spent, ceilStr, pctStr)
}

// settleSecsRemaining parses a RFC3339 settleUntil timestamp and returns the
// number of seconds remaining; 0 if expired or unparseable.
func (m *efficiencyModel) settleSecsRemaining(settleUntil string) int {
	// Use the snapshot's captured time as "now" proxy.
	now := m.snap.CapturedAt
	if now.IsZero() {
		return 0
	}
	// Import-free parse: use the standard library via time.Parse.
	// time is already imported transitively via the cockpit package types.
	// We cannot import time here directly without adding it to imports.
	// Use a helper instead.
	return settleSecsRemainingAt(settleUntil, now)
}

// settleSecsRemainingAt returns the seconds remaining in a settle window whose
// RFC3339 deadline is settleUntil, measured from now. Returns 0 if expired
// or if settleUntil cannot be parsed.
func settleSecsRemainingAt(settleUntil string, now time.Time) int {
	if settleUntil == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, settleUntil)
	if err != nil {
		return 0
	}
	remaining := t.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int(remaining.Seconds())
}

// renderSparklineFromFloats renders a fixed-width block-char sparkline from
// a float64 series. Reuses the block char encoding from cockpit.burndown.go.
func renderSparklineFromFloats(series []float64) string {
	const blockChars = " ▁▂▃▄▅▆▇█"
	runes := []rune(blockChars)
	nLevels := float64(len(runes) - 1)

	maxVal := 0.0
	for _, v := range series {
		if v > maxVal {
			maxVal = v
		}
	}

	var sb strings.Builder
	for _, v := range series {
		if maxVal == 0 {
			sb.WriteRune(' ')
			continue
		}
		idx := int(math.Round(v / maxVal * nLevels))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		sb.WriteRune(runes[idx])
	}
	return sb.String()
}

// renderTokenSection renders the Token Economy section: per-model rollup,
// per-bead token composition table (titles, most-recent first), cache-hit
// ratios + I7 tripwire, and the tokens-per-bead trend sparkline.
func (m *efficiencyModel) renderTokenSection(eff cockpit.EfficiencySnapshot) string {
	title := m.sectionTitle("Token Economy")

	var b strings.Builder

	// --- cache-hit ratios + tripwire -----------------------------------------
	if eff.FleetCacheHitRatio > 0 || len(eff.TokenRows) > 0 {
		allStr := lipgloss.NewStyle().Foreground(m.cacheHitColor(eff.FleetCacheHitRatio)).
			Render(fmt.Sprintf("%.1f%%", eff.FleetCacheHitRatio*100))
		recentStr := m.dimText("no dispatches in 24h")
		if eff.FleetCacheHit24h >= 0 {
			recentStr = lipgloss.NewStyle().Foreground(m.cacheHitColor(eff.FleetCacheHit24h)).
				Render(fmt.Sprintf("%.1f%%", eff.FleetCacheHit24h*100))
		}

		tripwireStr := ""
		if eff.CacheHitTripwire == "warn" {
			tripwireStr = "  " + lipgloss.NewStyle().Foreground(m.theme.Warning).Render("⚠ cache_read share collapsed — check prompt-prefix hygiene")
		}

		fmt.Fprintf(&b, "  cache-hit: %s last 24h · %s all-time%s\n", recentStr, allStr, tripwireStr)
	} else {
		b.WriteString(m.dimText("  no token data yet (accumulates from dispatches)") + "\n")
		return title + "\n" + b.String()
	}

	// --- tokens-per-bead trend sparkline -------------------------------------
	if len(eff.TokensPerBeadTrend) > 0 {
		spk := renderSparklineFromFloats(eff.TokensPerBeadTrend)
		// Find today's value.
		today := eff.TokensPerBeadTrend[len(eff.TokensPerBeadTrend)-1]
		fmt.Fprintf(&b, "  tokens/bead trend (%dd): %s  (%s today)\n",
			cockpit.SparklineLen, spk, formatTokenCount(int64(today)))
	}

	// --- per-model rollup ----------------------------------------------------
	// The "do I need to change models / serialize" table: token classes and
	// accumulated cost per serving model.
	numW := 8
	if len(eff.ModelRows) > 0 {
		modelW := 14
		header := m.tableHeader(
			modelW, "Model",
			5, "Beads",
			numW, "Total",
			numW, "Fresh",
			numW, "CacheR",
			numW, "Output",
			numW, "Cost",
		)
		b.WriteString(header + "\n")
		for _, mr := range eff.ModelRows {
			fmt.Fprintf(&b, "  %-*s  %-*d  %-*s  %-*s  %-*s  %-*s  $%.2f\n",
				modelW, truncate(shortModel(mr.Model), modelW),
				5, mr.Beads,
				numW, formatTokenCount(mr.TotalTokens),
				numW, formatTokenCount(mr.InputFresh),
				numW, formatTokenCount(mr.CacheRead),
				numW, formatTokenCount(mr.Output),
				mr.CostUSD,
			)
		}
	}

	// --- per-bead token composition table ------------------------------------
	if len(eff.TokenRows) == 0 {
		b.WriteString(m.dimText("  no per-bead token data") + "\n")
		return title + "\n" + b.String()
	}

	// Recent beads by TITLE (ids are in Detail); flexible title column.
	titleW := m.width - 5*numW - 7 - 14
	titleW = min(max(titleW, 16), 48)
	header := m.tableHeader(
		titleW, "Recent Bead",
		numW, "Total",
		numW, "Fresh",
		numW, "CacheR",
		numW, "CacheC",
		numW, "Output",
		7, "Hit%",
	)
	b.WriteString(header + "\n")

	for _, row := range eff.TokenRows {
		label := row.Title
		if label == "" {
			label = row.BeadID
		}
		bead := truncate(label, titleW)
		total := formatTokenCount(row.TotalTokens)
		fresh := formatTokenCount(row.InputFresh)
		cacheR := formatTokenCount(row.CacheRead)
		cacheC := formatTokenCount(row.CacheCreation)
		out := formatTokenCount(row.Output)
		hitPct := fmt.Sprintf("%.0f%%", row.CacheHitRatio*100)
		hitStyled := lipgloss.NewStyle().Foreground(m.cacheHitColor(row.CacheHitRatio)).Render(hitPct)

		fmt.Fprintf(&b, "  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
			titleW, bead,
			numW, total,
			numW, fresh,
			numW, cacheR,
			numW, cacheC,
			numW, out,
			hitStyled,
		)
	}

	return title + "\n" + b.String()
}

// cacheHitColor returns a color for a cache-hit ratio value.
// >=0.90 = green (healthy); >=0.80 = accent/blue (acceptable);
// >=0.60 = warning (degraded); <0.60 = error (I7 tripwire zone).
func (m *efficiencyModel) cacheHitColor(ratio float64) lipgloss.Color {
	switch {
	case ratio >= 0.90:
		return m.theme.Done
	case ratio >= 0.80:
		return m.theme.Accent
	case ratio >= 0.60:
		return m.theme.Warning
	default:
		return m.theme.Error
	}
}

// formatTokenCount formats a token count as a human-readable string with K/M suffix.
func formatTokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
