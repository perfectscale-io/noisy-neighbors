// Command noisy-wall serves the demo wall: a live picture of which tenants
// share which nodes, what each one promised, and what each one is actually
// consuming.
//
// It is a separate binary from the kubectl plugin on purpose. The plugin is
// read-only and stays that way; this one serves HTTP and, behind an explicit
// flag, can scale a lab deployment. Those belong nowhere near a tool people
// run against production clusters.
//
// Three modes, one renderer:
//
//	-synth          a scripted run of the lab, no cluster needed
//	-replay FILE    frames recorded earlier, played back at their own pace
//	(default)       live, polling the cluster in your kubeconfig
//
// Live and replay produce identical frames, which is what makes a recording a
// usable stage fallback: if the cluster fails mid-session, the same renderer
// shows the same shapes and nobody in the room can tell.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
	"github.com/perfectscale-io/noisy-neighbors/internal/collect"
	"github.com/perfectscale-io/noisy-neighbors/internal/wall"
)

func main() {
	var (
		listen       = flag.String("listen", "127.0.0.1:8099", "address to serve on; loopback unless you mean to share the wall")
		replayPath   = flag.String("replay", "", "replay frames from a JSONL file instead of polling a cluster")
		recordPath   = flag.String("record", "", "append every live frame to this JSONL file")
		synth        = flag.Bool("synth", false, "serve a scripted run of the lab; no cluster required")
		interval     = flag.Duration("interval", 5*time.Second, "how often to poll in live mode")
		sampleWin    = flag.Duration("sample-window", 4*time.Second, "kubelet sampling window for throttling rates")
		noKubelet    = flag.Bool("no-kubelet", false, "skip tier 2, the privileged kubelet reads")
		noMetrics    = flag.Bool("no-metrics", false, "skip tier 1, metrics-server")
		allowButton  = flag.Bool("allow-button", false, "permit scaling the lab aggressor from the UI")
		buttonNS     = flag.String("button-namespace", "team-payments", "namespace the button scales")
		buttonDeploy = flag.String("button-deployment", "ingest", "deployment the button scales")
		loop         = flag.Bool("loop", true, "in replay and synth modes, start over at the end")
		autoPress    = flag.Duration("auto-press", 0, "in synth mode, press the button automatically after this long (0: never)")
		kubeconfig   = flag.String("kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
		showTenants  = flag.String("tenants", "", "comma-separated namespaces to show on the simple view (default: every non-system namespace)")
	)
	flag.Parse()

	if *replayPath != "" && *synth {
		log.Fatal("choose one of -replay or -synth, not both")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := newBroker()
	srv := &server{broker: b, allowButton: *allowButton}
	// Scopes the simple view only. Collection stays cluster-wide so the node
	// totals on /detail remain true, and so the scan and the wall continue to
	// see the same cluster.
	if *showTenants != "" {
		for _, ns := range strings.Split(*showTenants, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				srv.showTenants = append(srv.showTenants, ns)
			}
		}
		log.Printf("simple view scoped to: %s", strings.Join(srv.showTenants, ", "))
	}

	switch {
	case *synth:
		log.Printf("mode: synthetic. No cluster will be contacted.")
		srv.synthetic = true
		go runSynth(ctx, b, srv, *loop, *autoPress)
	case *replayPath != "":
		log.Printf("mode: replay from %s", *replayPath)
		go func() {
			if err := runReplay(ctx, b, *replayPath, *loop); err != nil {
				log.Printf("replay stopped: %v", err)
			}
		}()
	default:
		log.Printf("mode: live, polling every %s", *interval)
		cfg, err := restConfig(*kubeconfig)
		if err != nil {
			log.Fatalf("building client config: %v\n\nTo run without a cluster: noisy-wall -synth", err)
		}
		clients, err := collect.NewClients(cfg)
		if err != nil {
			log.Fatalf("building clients: %v", err)
		}
		srv.clients = clients
		srv.buttonNS, srv.buttonDeploy = *buttonNS, *buttonDeploy
		opts := collect.Options{
			SampleWindow: *sampleWin,
			Concurrency:  8,
			NoKubelet:    *noKubelet,
			NoMetrics:    *noMetrics,
		}
		go runLive(ctx, b, clients, opts, *interval, *recordPath)
	}

	mux := http.NewServeMux()
	srv.routes(mux)

	httpSrv := &http.Server{
		Addr:         *listen,
		Handler:      guard(*listen, mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // server-sent events are a long-lived write
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if !*allowButton {
		log.Printf("button: disabled. Start with -allow-button to enable it.")
	}
	if !isLoopbackListen(*listen) {
		log.Printf("warning: listening on %s, so anyone who can reach this machine can see the wall", *listen)
		if *allowButton {
			log.Printf("warning: and they can press the button, which writes to the cluster as you")
		}
	}
	base := displayURL(*listen)
	log.Printf("bubbles: %s         (the front door)", base)
	log.Printf("bars:    %s/bars    (the same thing as rows)", base)
	log.Printf("detail:  %s/detail  (the console)", base)
	log.Printf("tenant:  %s/tenant  (the tenant's own dashboard)", base)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func restConfig(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	// A wall that hangs for the default 30s on an unreachable API server looks
	// broken rather than disconnected, and this lab's endpoint is IP-pinned.
	cfg.Timeout = 15 * time.Second
	return cfg, nil
}

// runLive polls the cluster and publishes a frame per cycle.
func runLive(ctx context.Context, b *broker, clients *collect.Clients,
	opts collect.Options, interval time.Duration, recordPath string) {

	var rec *recorder
	if recordPath != "" {
		r, err := newRecorder(recordPath)
		if err != nil {
			log.Printf("recording disabled: %v", err)
		} else {
			rec = r
			defer rec.Close()
			log.Printf("recording frames to %s", recordPath)
		}
	}

	th := analyze.DefaultThresholds()
	for {
		start := time.Now()
		res, err := collect.Collect(ctx, clients, opts)
		if err != nil {
			// A failed poll is not fatal. The endpoint may be briefly
			// unreachable, and a wall that exits on the first timeout is
			// useless on stage.
			log.Printf("collect failed: %v", err)
			if !sleepCtx(ctx, interval) {
				return
			}
			continue
		}
		rep := analyze.Run(res.Cluster, th)
		snap := wall.Build(res.Cluster, rep, th, "live")
		b.publish(snap)
		if rec != nil {
			rec.Write(snap)
		}
		if took := time.Since(start); took < interval {
			if !sleepCtx(ctx, interval-took) {
				return
			}
		} else if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// runSynth advances the scripted timeline in real time.
func runSynth(ctx context.Context, b *broker, srv *server, loop bool, autoPress time.Duration) {
	th := analyze.DefaultThresholds()
	const frame = 2 * time.Second
	const runFor = 150 * time.Second

	for {
		start := time.Now()
		srv.reset()
		s := wall.Synthetic{Start: start}

		for t := time.Duration(0); t < runFor; t += frame {
			if autoPress > 0 && t >= autoPress {
				srv.press()
			}
			cluster := s.Frame(t, srv.sinceButton())
			rep := analyze.Run(cluster, th)
			snap := wall.Build(cluster, rep, th, "synthetic")
			b.publish(snap)

			if !sleepCtx(ctx, frame) {
				return
			}
		}
		if !loop {
			return
		}
	}
}

// runReplay plays a JSONL recording back at the pace it was recorded.
func runReplay(ctx context.Context, b *broker, path string, loop bool) error {
	for {
		frames, err := loadFrames(path)
		if err != nil {
			return err
		}
		if len(frames) == 0 {
			return fmt.Errorf("%s contains no frames", path)
		}
		log.Printf("replaying %d frames", len(frames))

		for i, f := range frames {
			f.Source = "replay"
			f.Seq = int64(i + 1)
			b.publish(f)

			gap := 2 * time.Second
			if i+1 < len(frames) {
				if d := frames[i+1].At.Sub(f.At); d > 0 && d < 30*time.Second {
					gap = d
				}
			}
			if !sleepCtx(ctx, gap) {
				return nil
			}
		}
		if !loop {
			return nil
		}
	}
}

func loadFrames(path string) ([]*wall.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []*wall.Snapshot
	sc := bufio.NewScanner(f)
	// Frames carry every pod on every node, which outgrows the default 64KiB.
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s wall.Snapshot
		if err := json.Unmarshal(line, &s); err != nil {
			return nil, fmt.Errorf("frame %d: %w", len(out)+1, err)
		}
		out = append(out, &s)
	}
	return out, sc.Err()
}

type recorder struct {
	mu sync.Mutex
	f  *os.File
}

func newRecorder(path string) (*recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &recorder{f: f}, nil
}

func (r *recorder) Write(s *wall.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := json.NewEncoder(r.f).Encode(s); err != nil {
		log.Printf("recording frame failed: %v", err)
	}
}

func (r *recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
