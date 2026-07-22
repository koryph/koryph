// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// RenderPlan prints the plan in the repo's established plain-text table
// style (design §3.2): aligned `state`/`title`/`detail` columns via
// tabwriter, with a `needed` step's Why indented on its own continuation
// line under the detail column. Both cells of the continuation row are left
// empty (not just blank-padded strings) so tabwriter's own column-width
// computation — shared across every row written to tw before Flush — lines
// the "why:" text up under the detail column exactly as the design's example
// block shows.
func RenderPlan(w io.Writer, projectID, root string, steps []Step) {
	fmt.Fprintf(w, "ADOPTION PLAN — %s (%s)\n\n", projectID, root)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range steps {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.State, s.Title, s.Detail)
		if s.Why != "" && (s.State == StateNeeded || s.State == StateBlocked) {
			fmt.Fprintf(tw, "  \t\twhy: %s\n", s.Why)
		}
	}
	tw.Flush()
}
