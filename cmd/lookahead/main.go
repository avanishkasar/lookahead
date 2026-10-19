// Command lookahead sends DemandHints and runs the trace benchmark.
//
//	lookahead hint create -n NS --pool POOL --replicas N [--in 30s] [--ttl 5m]
//	lookahead hint list   -n NS [--pool POOL]
//	lookahead hint delete -n NS NAME
//	lookahead trace generate --out FILE [trace flags]
//	lookahead replay --trace FILE -n NS --pool POOL --strategy NAME --out FILE [--hints]
//	lookahead report --out FILE RESULT.json...
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
	"github.com/avanishkasar/lookahead/internal/replay"
	"github.com/avanishkasar/lookahead/internal/trace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctrllog.SetLogger(zap.New())
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `usage: lookahead <command> [flags]

commands:
  hint create|list|delete   manage DemandHints
  trace generate            write a synthetic claim trace (JSON Lines)
  replay                    replay a trace against the cluster and write results (JSON)
  report                    render results as a markdown benchmark table`

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "hint":
		if len(args) < 2 {
			return errors.New("usage: lookahead hint create|list|delete [flags]")
		}
		switch args[1] {
		case "create":
			return hintCreate(ctx, args[2:], out)
		case "list":
			return hintList(ctx, args[2:], out)
		case "delete":
			return hintDelete(ctx, args[2:], out)
		}
		return fmt.Errorf("unknown hint command %q", args[1])
	case "trace":
		if len(args) < 2 || args[1] != "generate" {
			return errors.New("usage: lookahead trace generate [flags]")
		}
		return traceGenerate(args[2:], out)
	case "replay":
		return replayCmd(ctx, args[1:], out)
	case "report":
		return reportCmd(args[1:], out)
	case "-h", "--help", "help":
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}

func newClient() (client.WithWatch, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, sandboxv1beta1.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return client.NewWithWatch(cfg, client.Options{Scheme: s})
}

func hintCreate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hint create", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace of the pool")
	pool := fs.String("pool", "", "SandboxWarmPool name (required)")
	replicas := fs.Int("replicas", 0, "sandboxes needed (required, 1-10000)")
	in := fs.Duration("in", 0, "when the sandboxes are needed, from now")
	ttl := fs.Duration("ttl", 5*time.Minute, "how long after the needed time the hint counts")
	name := fs.String("name", "", "hint name (default: generated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pool == "" || *replicas < 1 || *replicas > 10000 {
		return errors.New("--pool and --replicas (1-10000) are required")
	}
	if *in < 0 || *ttl < time.Second || *ttl > 24*time.Hour {
		return errors.New("--in must not be negative and --ttl must be between 1s and 24h")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	h := &v1alpha1.DemandHint{
		ObjectMeta: metav1.ObjectMeta{Namespace: *ns, Name: *name},
		Spec: v1alpha1.DemandHintSpec{
			PoolRef:    v1alpha1.PoolReference{Name: *pool},
			Replicas:   int32(*replicas),
			NeededAt:   &metav1.Time{Time: time.Now().Add(*in)},
			TTLSeconds: new(int32(ttl.Seconds())),
		},
	}
	if *name == "" {
		h.GenerateName = *pool + "-"
	}
	if err := c.Create(ctx, h); err != nil {
		return fmt.Errorf("create DemandHint: %w", err)
	}
	_, err = fmt.Fprintf(out, "demandhint/%s created (pool %s, %d replicas, needed at %s, expires %s)\n",
		h.Name, *pool, *replicas, h.NeededTime().Format(time.RFC3339), h.ExpiryTime().Format(time.RFC3339))
	return err
}

func hintList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hint list", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace")
	pool := fs.String("pool", "", "only hints for this pool")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	list := &v1alpha1.DemandHintList{}
	if err := c.List(ctx, list, client.InNamespace(*ns)); err != nil {
		return fmt.Errorf("list DemandHints: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPOOL\tREPLICAS\tNEEDED\tEXPIRES")
	now := time.Now()
	for _, h := range list.Items {
		if *pool != "" && h.Spec.PoolRef.Name != *pool {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", h.Name, h.Spec.PoolRef.Name, h.Spec.Replicas,
			relative(h.NeededTime(), now), relative(h.ExpiryTime(), now))
	}
	return tw.Flush()
}

func relative(t, now time.Time) string {
	d := t.Sub(now).Round(time.Second)
	if d >= 0 {
		return "in " + d.String()
	}
	return (-d).String() + " ago"
}

func hintDelete(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hint delete", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: lookahead hint delete -n NS NAME")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	for _, name := range fs.Args() {
		h := &v1alpha1.DemandHint{ObjectMeta: metav1.ObjectMeta{Namespace: *ns, Name: name}}
		if err := c.Delete(ctx, h); err != nil {
			return fmt.Errorf("delete DemandHint %s: %w", name, err)
		}
		fmt.Fprintf(out, "demandhint/%s deleted\n", name)
	}
	return nil
}

func traceGenerate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("trace generate", flag.ContinueOnError)
	var c trace.Config
	path := fs.String("out", "", "output file (required)")
	fs.Uint64Var(&c.Seed, "seed", 1, "random seed")
	fs.DurationVar(&c.Duration, "duration", 10*time.Minute, "trace length")
	fs.Float64Var(&c.BaseRate, "base-rate", 0.1, "background Poisson rate, claims/s")
	fs.DurationVar(&c.BurstEvery, "burst-every", 2*time.Minute, "mean time between bursts (0 disables)")
	fs.Float64Var(&c.BurstJitter, "burst-jitter", 0.1, "burst start jitter as a fraction of --burst-every, [0, 0.5)")
	fs.IntVar(&c.BurstSize, "burst-size", 20, "claims per burst")
	fs.DurationVar(&c.BurstDuration, "burst-duration", 5*time.Second, "time over which a burst's claims arrive")
	fs.DurationVar(&c.Hold, "hold", time.Minute, "mean claim hold time")
	fs.Float64Var(&c.HintFraction, "hint-fraction", 1, "fraction of bursts announced by a hint")
	fs.DurationVar(&c.HintLead, "hint-lead", 30*time.Second, "how long before a burst its hint is sent")
	fs.DurationVar(&c.HintTTL, "hint-ttl", 2*time.Minute, "hint TTL after the burst start")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--out is required")
	}
	events, err := trace.Generate(c)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(*path, func(w io.Writer) error { return trace.Write(w, events) }); err != nil {
		return err
	}
	claims := 0
	for _, e := range events {
		if e.Kind == trace.KindClaim {
			claims++
		}
	}
	_, err = fmt.Fprintf(out, "wrote %s: %d claims, %d hints\n", *path, claims, len(events)-claims)
	return err
}

func replayCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	tracePath := fs.String("trace", "", "trace file (required)")
	var cfg replay.Config
	fs.StringVar(&cfg.Namespace, "n", "default", "namespace of the pool")
	fs.StringVar(&cfg.Pool, "pool", "", "SandboxWarmPool to claim from (required)")
	fs.StringVar(&cfg.Strategy, "strategy", "", "strategy label for the report (required)")
	fs.StringVar(&cfg.RunID, "run-id", "", "label value for created objects (default: generated)")
	fs.BoolVar(&cfg.UseHints, "hints", false, "replay the trace's hint events as DemandHints")
	fs.DurationVar(&cfg.ReadyTimeout, "ready-timeout", 5*time.Minute, "give up waiting for a claim after this long")
	fs.DurationVar(&cfg.SampleInterval, "sample-interval", time.Second, "pool sampling interval")
	outPath := fs.String("out", "", "result file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tracePath == "" || cfg.Pool == "" || cfg.Strategy == "" || *outPath == "" {
		return errors.New("--trace, --pool, --strategy and --out are required")
	}
	if cfg.RunID == "" {
		cfg.RunID = fmt.Sprintf("la-%d", time.Now().Unix())
	}
	cfg.TraceName = strings.TrimSuffix(filepath.Base(*tracePath), filepath.Ext(*tracePath))
	f, err := os.Open(*tracePath)
	if err != nil {
		return err
	}
	events, err := trace.Read(f)
	f.Close()
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	res, err := replay.Run(ctx, c, events, cfg)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(*outPath, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}); err != nil {
		return err
	}
	s := res.Summary
	_, err = fmt.Fprintf(out, "%s/%s: %d claims, p50 %.0fms p95 %.0fms p99 %.0fms, cold %.1f%%, timed out %d, idle %.1f sandbox-min -> %s\n",
		cfg.TraceName, cfg.Strategy, s.Claims, s.P50Ms, s.P95Ms, s.P99Ms, s.ColdStartRate*100, s.TimedOut, s.IdleSandboxMinutes, *outPath)
	return err
}

func reportCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	outPath := fs.String("out", "", "markdown file (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: lookahead report [--out FILE] RESULT.json [RESULT.json]")
	}
	var results []*trace.Result
	for _, p := range fs.Args() {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		r := &trace.Result{}
		if err := json.Unmarshal(b, r); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		results = append(results, r)
	}
	if *outPath == "" {
		return trace.WriteReport(out, results)
	}
	return writeFileAtomic(*outPath, func(w io.Writer) error { return trace.WriteReport(w, results) })
}

// writeFileAtomic writes to a temp file next to path and renames it, so an interrupted run
// never leaves a truncated result behind.
func writeFileAtomic(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
