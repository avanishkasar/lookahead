package inspect

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Style controls terminal decoration of a Card.
type Style struct {
	// Color enables ANSI colors. Leave it off for MCP results and pipes.
	Color bool
}

const (
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
	ansiCyan   = "36"
)

func (st Style) paint(code, s string) string {
	if !st.Color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Brief is one or two sentences saying what lookahead is doing to the pool and why.
func Brief(s *Snapshot) string {
	if !s.PoolFound {
		return fmt.Sprintf("SandboxWarmPool %s/%s not found.", s.Namespace, s.Pool)
	}
	ready := fmt.Sprintf("%s: %d of %d warm sandboxes ready", s.Pool, s.Ready, s.Spec)
	p := s.Policy
	switch {
	case p == nil:
		return ready + ". No LookaheadPolicy targets this pool, so lookahead is not sizing it."
	case !p.Reconciled:
		return fmt.Sprintf("%s. Policy %s has not been reconciled yet; check that lookahead-controller is running.", ready, p.Name)
	case p.Conflict != "":
		return fmt.Sprintf("%s. lookahead is not scaling it: %s.", ready, p.Conflict)
	case !p.Ready:
		return fmt.Sprintf("%s. Policy %s is not ready: %s.", ready, p.Name, p.Message)
	}
	b := fmt.Sprintf("%s; lookahead targets %d = forecast %d + hints %d (%d active), clamped to [%d, %d].",
		ready, p.Desired, p.Forecast, p.HintReplicas, p.ActiveHints, p.Min, p.Max)
	if p.Holding() {
		b += fmt.Sprintf(" Holding at %d through the %d s scale-down window; demand alone asks for %d.",
			p.Desired, p.StabilizationSeconds, p.Raw())
	}
	for _, h := range s.Hints {
		if h.Phase(s.At) == PhasePending {
			b += fmt.Sprintf(" Next: +%d from %s in %s.", h.Replicas, h.Name, Short(h.CountsFrom.Sub(s.At)))
			break
		}
	}
	return b
}

const (
	barWidth   = 36
	sparkWidth = 30
	maxHints   = 5
)

// Card writes a multi-line view of the pool. h may be nil; then the history rows show only what
// the snapshot itself holds.
func Card(w io.Writer, s *Snapshot, h *History, st Style) error {
	var b strings.Builder
	row := func(label, body string) {
		fmt.Fprintf(&b, "%s %s %s\n", st.paint(ansiDim, "│"), st.paint(ansiDim, fmt.Sprintf("%-9s", label)), body)
	}
	fmt.Fprintf(&b, "%s %s  %s  %s\n", st.paint(ansiDim, "╭─"), st.paint(ansiBold, s.Pool),
		st.paint(ansiDim, s.Namespace), st.paint(ansiDim, s.At.Format("15:04:05")))
	if !s.PoolFound {
		row("pool", st.paint(ansiRed, "SandboxWarmPool not found"))
		b.WriteString(st.paint(ansiDim, "╰─") + "\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	p := s.Policy
	scale := max(s.Spec, s.Replicas, s.Ready, 1)
	if p != nil {
		scale = max(scale, p.Max)
	}
	row("ready", poolBar(s, scale, st)+"  "+
		fmt.Sprintf("%s ready · %s target · max %d", st.paint(ansiBold, fmt.Sprint(s.Ready)), st.paint(ansiBold, fmt.Sprint(s.Spec)), scale))

	switch {
	case p == nil:
		row("policy", st.paint(ansiYellow, "none: lookahead is not sizing this pool"))
	case !p.Reconciled:
		row("policy", st.paint(ansiYellow, p.Name+" not reconciled yet: is lookahead-controller running?"))
	case p.Conflict != "":
		row("policy", st.paint(ansiRed, "not scaling: "+p.Conflict))
	case !p.Ready:
		row("policy", st.paint(ansiRed, p.Name+" not ready: "+p.Message))
	default:
		row("target", fmt.Sprintf("%s = forecast %d + hints %s (%d active) · clamp [%d, %d]",
			st.paint(ansiBold, fmt.Sprint(p.Desired)), p.Forecast, st.paint(ansiCyan, fmt.Sprint(p.HintReplicas)), p.ActiveHints, p.Min, p.Max))
		if p.Holding() {
			row("", st.paint(ansiYellow, fmt.Sprintf("holding for scale-down: demand asks %d, window %d s", p.Raw(), p.StabilizationSeconds)))
		}
		forecast := p.Model + fmt.Sprintf(" · %d s lead", p.LeadSeconds)
		if p.Model == "None" {
			forecast = fmt.Sprintf("off (hints only) · %d s lead", p.LeadSeconds)
		}
		row("forecast", forecast)
	}

	claims := s.Claims
	if h != nil {
		claims = h.Claims()
	}
	lastMin, waiting := 0, 0
	for _, c := range claims {
		if !c.Created.Before(s.At.Add(-time.Minute)) {
			lastMin++
		}
		if !c.Ready {
			waiting++
		}
	}
	claimLine := fmt.Sprintf("%d in the last minute", lastMin)
	if waiting > 0 {
		claimLine += " · " + st.paint(ansiRed, fmt.Sprintf("%d waiting for a sandbox", waiting))
	}
	row("claims", claimLine)

	if len(s.Hints) == 0 {
		row("hints", st.paint(ansiDim, "none"))
	}
	for i, hn := range s.Hints {
		if i == maxHints {
			row("", st.paint(ansiDim, fmt.Sprintf("… %d more", len(s.Hints)-maxHints)))
			break
		}
		label := ""
		if i == 0 {
			label = "hints"
		}
		row(label, hintLine(hn, s.At, st))
	}

	if h != nil {
		row("history", "ready  "+st.paint(ansiGreen, readySpark(h.Samples(), s.At, scale))+st.paint(ansiDim, "  last 2m"))
	}
	row("", "claims "+st.paint(ansiCyan, claimSpark(claims, s.At))+st.paint(ansiDim, "  per 10s, last 5m"))
	b.WriteString(st.paint(ansiDim, "╰─") + "\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// poolBar draws Ready sandboxes as █, the rest of the target as ▒ and headroom up to scale as ░.
func poolBar(s *Snapshot, scale int32, st Style) string {
	cells := func(n int32) int { return int(math.Round(float64(n) / float64(scale) * barWidth)) }
	ready := min(cells(s.Ready), barWidth)
	starting := min(max(cells(s.Spec)-ready, 0), barWidth-ready)
	return st.paint(ansiGreen, strings.Repeat("█", ready)) +
		st.paint(ansiYellow, strings.Repeat("▒", starting)) +
		st.paint(ansiDim, strings.Repeat("░", barWidth-ready-starting))
}

func hintLine(h Hint, now time.Time, st Style) string {
	name := fmt.Sprintf("%-20s +%-5d", truncate(h.Name, 20), h.Replicas)
	switch h.Phase(now) {
	case PhasePending:
		return st.paint(ansiCyan, "○ ") + name + fmt.Sprintf("pending  counts in %s · needed in %s",
			Short(h.CountsFrom.Sub(now)), Short(h.NeededAt.Sub(now)))
	case PhaseActive:
		needed := "needed in " + Short(h.NeededAt.Sub(now))
		if !now.Before(h.NeededAt) {
			needed = "needed " + Short(now.Sub(h.NeededAt)) + " ago"
		}
		return st.paint(ansiCyan, "● ") + name + st.paint(ansiCyan, "active") + fmt.Sprintf("   %s · expires in %s",
			needed, Short(h.Expires.Sub(now)))
	}
	return st.paint(ansiDim, "✕ "+name+"expired "+Short(now.Sub(h.Expires))+" ago")
}

const sparkGlyphs = "▁▂▃▄▅▆▇█"

// spark maps values in [0, top] onto eighth-block glyphs; negative values render as blanks.
func spark(values []float64, top float64) string {
	glyphs := []rune(sparkGlyphs)
	var b strings.Builder
	for _, v := range values {
		switch {
		case v < 0:
			b.WriteRune(' ')
		case top <= 0:
			b.WriteRune(glyphs[0])
		default:
			b.WriteRune(glyphs[int(math.Round(min(v/top, 1)*float64(len(glyphs)-1)))])
		}
	}
	return b.String()
}

// readySpark shows Ready sandboxes over the last two minutes, carrying each sample forward.
func readySpark(samples []Sample, now time.Time, scale int32) string {
	const span = 2 * time.Minute
	step := span / sparkWidth
	values := make([]float64, sparkWidth)
	j, last := 0, -1.0
	for i := range values {
		end := now.Add(-span + time.Duration(i+1)*step)
		for j < len(samples) && !samples[j].At.After(end) {
			last = float64(samples[j].Ready)
			j++
		}
		values[i] = last
	}
	return spark(values, float64(scale))
}

// claimSpark counts claim arrivals per 10 s over the last five minutes.
func claimSpark(claims []Claim, now time.Time) string {
	const span = 5 * time.Minute
	step := span / sparkWidth
	values := make([]float64, sparkWidth)
	top := 0.0
	for _, c := range claims {
		age := now.Sub(c.Created)
		if age < 0 || age >= span {
			continue
		}
		i := sparkWidth - 1 - int(age/step)
		values[i]++
		top = max(top, values[i])
	}
	return spark(values, top)
}

// Short formats d as a compact duration such as 45s, 1m05s or 2h03m.
func Short(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
