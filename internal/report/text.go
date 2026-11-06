package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Text renders the report for a person: peers by name, channels by what they
// earn, amounts rounded, decisions in plain words, and a few facts worth a
// look. Raw gives every exact value.
func (r *Report) Text() string {
	var b strings.Builder
	t := r.Totals
	days := r.WindowHours / 24
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-12s%s\n", label, fmt.Sprintf(format, args...))
	}

	fmt.Fprintf(&b, "Lightning Jet report for the %d days to %s\n\n", days, r.TakenAt.Format("2006-01-02 15:04 MST"))
	active := "all active"
	if t.Active < t.Channels {
		active = fmt.Sprintf("%d active", t.Active)
	}
	line("Node", "%d channels, %s. %s sats of capacity, %d%% of it on our side.", t.Channels, active, sats(t.CapacityMsat), pct(t.LocalMsat, t.CapacityMsat))
	line("Earned", "%s sats in fees from %d forwards (%s sats routed): %s a year on capacity.",
		sats(t.OutFeeMsat), t.OutCount, sats(t.OutAmountMsat), yieldText(t.YieldPct))
	if t.CoverageHours > 0 || t.BalanceFailures > 0 {
		line("Missed", "%d forwards failed for lack of balance on our side, %s sats in all.", t.BalanceFailures, sats(t.BalanceFailuresMsat))
		line("", "Recorded for %d of the %d hours. These are attempts, not payments.", t.CoverageHours, r.WindowHours)
	}
	switch t.Forecasts {
	case 0:
		line(fmt.Sprintf("Next %dd", t.HorizonHours/24), "No channel has enough history to forecast.")
	case 1:
		line(fmt.Sprintf("Next %dd", t.HorizonHours/24), "%s sats expected, from the one channel with enough history to forecast.",
			sats(t.ExpectedMsat))
	default:
		line(fmt.Sprintf("Next %dd", t.HorizonHours/24), "%s sats expected, from the %d channels with enough history to forecast.",
			sats(t.ExpectedMsat), t.Forecasts)
	}

	chans := byEarnings(r.Channels)
	if len(chans) > 0 {
		b.WriteString("\n")
		table(&b, chans, t.HorizonHours/24)
	}

	if notes := worthALook(chans, t, days); len(notes) > 0 {
		b.WriteString("\nWorth a look\n")
		for _, n := range notes {
			b.WriteString(wrap("- "+n, "  ", 80))
		}
	}

	b.WriteString("\n" + wrap(fmt.Sprintf("Out fee is what we charge to forward out through the channel. Others is the "+
		"median out fee of the other channels into the same peer: a prior, not a price. Both are in ppm. Earned and "+
		"Missed cover the %d days, and Next %dd is the forecast. Amounts are in sats.", days, t.HorizonHours/24), "", 80))
	b.WriteString(wrap("Shadow mode: Jet changed nothing. A change it approves isn't applied, so it proposes the "+
		"change again, and the limits count each approval as if it had been.", "", 80))
	b.WriteString("jet explain <channel> explains a row; jet report --raw gives exact values.\n")
	return b.String()
}

// byEarnings orders channels by the fees they earned, then by forwards, then
// by capacity, so the ones that matter come first.
func byEarnings(in []Channel) []Channel {
	out := append([]Channel(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Forwards.OutFeeMsat != b.Forwards.OutFeeMsat:
			return a.Forwards.OutFeeMsat > b.Forwards.OutFeeMsat
		case a.Forwards.OutCount != b.Forwards.OutCount:
			return a.Forwards.OutCount > b.Forwards.OutCount
		default:
			return a.CapacityMsat > b.CapacityMsat
		}
	})
	return out
}

const nameWidth = 18

func table(b *strings.Builder, chans []Channel, horizonDays int64) {
	cols := []struct {
		head  string
		right bool
	}{{"Peer", false}, {"Channel", false}, {"Our side", false}, {"Out fee", true}, {"Others", true},
		{"Earned", true}, {"Missed", true}, {fmt.Sprintf("Next %dd", horizonDays), true}, {"Jet", false}}
	rows := [][]string{}
	for _, c := range chans {
		fee, others, missed, next := "-", "-", "-", "-"
		if c.FeePPM != nil {
			fee = strconv.FormatInt(*c.FeePPM, 10)
		}
		if c.Competition != nil && c.Competition.Channels > 0 {
			others = strconv.FormatInt(c.Competition.FeePPMMedian, 10)
		}
		if c.Failures != nil {
			missed = strconv.Itoa(c.Failures.BalanceFailures)
		}
		if c.Forecast != nil {
			next = sats(c.Forecast.ExpectedMsat)
		}
		p := pct(c.LocalMsat, c.CapacityMsat)
		rows = append(rows, []string{truncate(name(c), nameWidth), c.SCID, fmt.Sprintf("%s %3d%%", bar(p), p),
			fee, others, sats(c.Forwards.OutFeeMsat), missed, next, decision(c)})
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = width(c.head)
		for _, r := range rows {
			widths[i] = max(widths[i], width(r[i]))
		}
	}
	write := func(cells []string) {
		var line strings.Builder
		for i, cell := range cells {
			if i == len(cells)-1 {
				line.WriteString(cell) // the last column isn't padded
				break
			}
			pad := strings.Repeat(" ", widths[i]-width(cell))
			if cols[i].right {
				line.WriteString(pad + cell)
			} else {
				line.WriteString(cell + pad)
			}
			line.WriteString("  ")
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	heads := make([]string, len(cols))
	for i, c := range cols {
		heads[i] = c.head
	}
	write(heads)
	for _, r := range rows {
		write(r)
	}
}

// bar draws the share of the channel's capacity on our side, in tenths.
func bar(p int64) string {
	full := int((p + 5) / 10)
	return strings.Repeat("█", full) + strings.Repeat("░", 10-full)
}

// decision says what Jet decided about the channel in plain words. In shadow
// mode nothing is applied, so a proposal is what Jet would do.
func decision(c Channel) string {
	s := "no decision recorded"
	switch {
	case c.Abstained == contracts.InsufficientEvidence:
		s = "too few forwards to judge"
	case c.Abstained == contracts.NoProfitableAction:
		s = "keep the fee: no better one expected"
	case c.Abstained == contracts.InsufficientData:
		s = "data incomplete, so no decision"
	case c.Abstained != "":
		s = strings.ReplaceAll(string(c.Abstained), "_", " ")
	case c.Proposal != nil:
		verb := "raise"
		if c.Proposal.FeePPM < c.Proposal.ObservedFeePPM {
			verb = "lower"
		}
		s = fmt.Sprintf("would %s to %d ppm", verb, c.Proposal.FeePPM)
		if c.Verdict == contracts.Rejected {
			s += "; " + held(c)
		}
	}
	if !c.Active {
		s = "inactive; " + s
	}
	return s
}

// ruleWords names the authorizer's rules for a reader.
var ruleWords = map[string]string{
	"mode": "fees mode", "expiry": "expired", "freeze": "frozen", "fee_ppm_bounds": "fee bounds",
	"fee_step": "step limit", "cooldown": "cooldown", "daily_changes": "daily limit",
}

// historyRules are the limits that count earlier approvals.
var historyRules = map[string]bool{"cooldown": true, "daily_changes": true}

// held says why a proposal wasn't approved. In shadow mode, one the limits
// hold for its history was approved before and never applied, which is why
// the fee is unchanged and Jet proposes it again: say that, not that it was
// held.
func held(c Channel) string {
	var other []string
	history := false
	for _, r := range c.Blocked {
		if historyRules[r] {
			history = true
		} else {
			other = append(other, r)
		}
	}
	if !history || c.Mode != contracts.ModeShadow || c.PriorApprovals == 0 {
		return "held: " + heldBy(c.Blocked)
	}
	s := "already approved " + timesWord(c.PriorApprovals) + " in shadow in the last 24h"
	if len(other) > 0 {
		s += "; held: " + heldBy(other)
	}
	return s
}

func timesWord(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// heldBy lists the rules a proposal failed, as words.
func heldBy(rules []string) string {
	if len(rules) == 0 {
		return "the limits"
	}
	words := make([]string, len(rules))
	for i, r := range rules {
		if w, ok := ruleWords[r]; ok {
			words[i] = w
		} else {
			words[i] = strings.ReplaceAll(r, "_", " ")
		}
	}
	return strings.Join(words, ", ")
}

// worthALook picks out facts in the report that call for attention. Each is
// stated as observed, without a recommendation.
func worthALook(chans []Channel, t Totals, days int64) []string {
	var notes []string
	for _, c := range chans {
		p := pct(c.LocalMsat, c.CapacityMsat)
		if c.Failures == nil || c.Failures.BalanceFailures == 0 || p > 10 {
			continue
		}
		n := fmt.Sprintf("%s is nearly empty on our side (%d%%), and %d forwards out of it failed for lack of balance.",
			name(c), p, c.Failures.BalanceFailures)
		if t.OutFeeMsat > 0 {
			if share := int64(c.Forwards.OutFeeMsat) * 100 / int64(t.OutFeeMsat); share >= 10 {
				n = fmt.Sprintf("%s earns %d%% of the fees but is nearly empty on our side (%d%%): %d forwards out of it failed for lack of balance.",
					name(c), share, p, c.Failures.BalanceFailures)
			}
		}
		notes = append(notes, n)
	}

	var idle []string
	var idleLocal contracts.MilliSat
	for _, c := range chans {
		if c.Forwards.OutCount == 0 && pct(c.LocalMsat, c.CapacityMsat) >= 50 {
			idle = append(idle, name(c))
			idleLocal += c.LocalMsat
		}
	}
	if len(idle) > 0 {
		who := idle
		if len(who) > 3 {
			who = append(who[:3:3], fmt.Sprintf("%d more", len(idle)-3))
		}
		subject := fmt.Sprintf("%d channels", len(idle))
		if len(idle) == 1 {
			subject = "1 channel"
		}
		notes = append(notes, fmt.Sprintf("%s forwarded nothing out in %d days while holding %s sats on our side: %s.",
			subject, days, sats(idleLocal), strings.Join(who, ", ")))
	}

	for _, c := range chans {
		if !c.Active {
			notes = append(notes, fmt.Sprintf("%s is inactive: its peer is offline or unreachable.", name(c)))
		}
	}
	return notes
}

// name is the peer's alias made safe to print, or the start of its public key
// when it announced none.
func name(c Channel) string {
	if a := clean(c.PeerAlias); a != "" {
		return a
	}
	if len(c.Peer) > 12 {
		return c.Peer[:12] + "…"
	}
	return c.Peer
}

// clean removes what a peer could put in its alias to rewrite a terminal:
// control characters, escape sequences, and formatting marks such as bidi
// overrides. Runs of space become one.
func clean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar:
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncate shortens s to at most w terminal columns.
func truncate(s string, w int) string {
	if width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		if used+runeWidth(r) > w-1 {
			break
		}
		b.WriteRune(r)
		used += runeWidth(r)
	}
	return b.String() + "…"
}

// width is how many terminal columns s takes.
func width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// runeWidth is 0 for combining marks, 2 for wide characters (CJK and emoji),
// and 1 otherwise: the common cases, not every terminal's rules.
func runeWidth(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me):
		return 0
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F680 && r <= 0x1F6FF, r >= 0x1F900 && r <= 0x1F9FF, r >= 0x1FA70 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	case strings.ContainsRune("⌚⌛⏩⏪⏫⏬⏰⏳◽◾☔☕♈♉♊♋♌♍♎♏♐♑♒♓♿⚓⚡⚪⚫⚽⚾⛄⛅⛎⛔⛪⛲⛳⛵⛺⛽✅✊✋✨❌❎❓❔❕❗➕➖➗➰➿⬛⬜⭐⭕", r):
		return 2 // symbols terminals show as emoji
	}
	return 1
}

// wrap breaks text into lines of at most w columns, indenting the ones after
// the first.
func wrap(text, indent string, w int) string {
	var b strings.Builder
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case width(line)+1+width(word) > w:
			b.WriteString(line + "\n")
			line = indent + word
		default:
			line += " " + word
		}
	}
	if line != "" {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// sats writes an amount in whole sats, exactly below 100,000 and in millions
// above, which is as much precision as a reader of this view needs.
func sats(m contracts.MilliSat) string {
	n := m.Sats()
	if n < 100_000 && n > -100_000 {
		return commas(n)
	}
	s := strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + "M"
}

func yieldText(pctPerYear float64) string {
	return strconv.FormatFloat(pctPerYear, 'f', 2, 64) + "%"
}

func pct(part, whole contracts.MilliSat) int64 {
	if whole <= 0 {
		return 0
	}
	return int64(part) * 100 / int64(whole)
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
