package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/avanishkasar/lookahead/internal/dashboard"
	"github.com/avanishkasar/lookahead/internal/inspect"
	"github.com/avanishkasar/lookahead/internal/mcpserver"
)

// useColor reports whether out is a terminal and the user has not opted out of color.
func useColor(out io.Writer, noColor bool) bool {
	f, ok := out.(*os.File)
	return ok && !noColor && os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(f.Fd()))
}

// cards renders a brief and a card for each pool, recording into hist when it is non-nil.
func cards(ctx context.Context, c client.Reader, ns, pool string, hist map[string]*inspect.History, st inspect.Style) ([]byte, error) {
	pools := []string{pool}
	if pool == "" {
		var err error
		if pools, err = inspect.Pools(ctx, c, ns); err != nil {
			return nil, err
		}
		if len(pools) == 0 {
			return []byte(fmt.Sprintf("No SandboxWarmPools in namespace %s.\n", ns)), nil
		}
	}
	var b bytes.Buffer
	now := time.Now()
	for i, p := range pools {
		snap, err := inspect.Collect(ctx, c, ns, p, now)
		if err != nil {
			return nil, err
		}
		var h *inspect.History
		if hist != nil {
			if h = hist[p]; h == nil {
				h = inspect.NewHistory(inspect.ClaimWindow)
				hist[p] = h
			}
			h.Observe(snap)
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(inspect.Brief(snap) + "\n")
		if err := inspect.Card(&b, snap, h, st); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func statusCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace")
	pool := fs.String("pool", "", "only this SandboxWarmPool (default: every pool lookahead manages)")
	noColor := fs.Bool("no-color", false, "disable colors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	b, err := cards(ctx, c, *ns, *pool, nil, inspect.Style{Color: useColor(out, *noColor)})
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

func watchCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace")
	pool := fs.String("pool", "", "only this SandboxWarmPool (default: every pool lookahead manages)")
	interval := fs.Duration("interval", time.Second, "refresh interval")
	noColor := fs.Bool("no-color", false, "disable colors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval < 100*time.Millisecond {
		return errors.New("--interval must be at least 100ms")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	st := inspect.Style{Color: useColor(out, *noColor)}
	// Redraw in place on a terminal; elsewhere append frames so logs stay readable.
	tty := st.Color || useColor(out, false)
	if tty {
		fmt.Fprint(out, "\x1b[?25l\x1b[2J")
		defer fmt.Fprint(out, "\x1b[?25h")
	}
	hist := map[string]*inspect.History{}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		b, err := cards(ctx, c, *ns, *pool, hist, st)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b = []byte("error: " + err.Error() + "\n")
		}
		footer := fmt.Sprintf("\nupdated %s · every %s · Ctrl+C to quit\n", time.Now().Format("15:04:05"), *interval)
		if tty {
			fmt.Fprintf(out, "\x1b[H%s%s\x1b[J", b, footer)
		} else {
			fmt.Fprintf(out, "%s%s", b, footer)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func dashboardCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace")
	pool := fs.String("pool", "", "only this SandboxWarmPool (default: every pool lookahead manages)")
	addr := fs.String("addr", "127.0.0.1:8484", "listen address; keep it on localhost unless you mean to share the view")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	if host, _, _ := net.SplitHostPort(ln.Addr().String()); !net.ParseIP(host).IsLoopback() {
		fmt.Fprintf(os.Stderr, "warning: dashboard is reachable from other machines on %s\n", ln.Addr())
	}
	d := dashboard.New(c, dashboard.Config{Namespace: *ns, Pool: *pool})
	go d.Run(ctx)
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(out, "lookahead dashboard on http://%s (namespace %s) · Ctrl+C to stop\n", ln.Addr(), *ns)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func mcpCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	ns := fs.String("n", "default", "namespace used when a tool call names none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	version := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	srv := mcpserver.New(c, mcpserver.Config{Namespace: *ns, Version: version, PickupTimeout: 5 * time.Second})
	// stdout carries the protocol; anything else this process prints goes to stderr.
	return srv.Run(ctx, &mcp.StdioTransport{})
}
