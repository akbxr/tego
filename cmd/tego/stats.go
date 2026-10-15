package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/stats"
)

const statsUsage = `tego stats: usage statistics from every tego session

Usage:
  tego stats [-r range] [-j]         summary in the terminal (-j, --json: as JSON)
  tego stats serve [-r range] [-p n] dashboard in the browser
Flags:
  -r, --range <24h|7d|30d|90d|all>  time range (default: all; the dashboard opens at 30d)
  -j, --json                        print the report as JSON
  -p, --port <n>                    dashboard port (default 3847, or a free one)
      --host <addr>                 dashboard address (default 127.0.0.1)
`

// recordStats logs every model request and tool call of this process to
// ~/.tego/stats.jsonl for tego stats.
func recordStats(cfg *config.Config, cwd string, ag *agent.Agent, ui *termUI) {
	log := stats.OpenLog(cfg.Dir)
	llm.SetRequestHook(func(st llm.RequestStat) {
		log.Add(stats.RequestRecord(st, cwd))
		if st.Class == "" || st.Class == llm.ClassMain {
			ui.requestDone(st)
		}
	})
	ag.ToolStat = func(name string, isErr, subagent bool) {
		class := llm.ClassMain
		if subagent {
			class = llm.ClassSubagent
		}
		log.Add(stats.Record{Kind: "tool", TS: time.Now().UnixMilli(), Folder: cwd, Class: class, Tool: name, ToolError: isErr})
	}
}

// statsFlags parses the flags of tego stats and tego stats serve.
func statsFlags(args []string, serve bool) (rng string, asJSON bool, host string, port int, err error) {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, n := range []string{"r", "range"} {
		fs.StringVar(&rng, n, "", "")
	}
	if serve {
		for _, n := range []string{"p", "port"} {
			fs.IntVar(&port, n, 0, "")
		}
		fs.StringVar(&host, "host", "127.0.0.1", "")
	} else {
		for _, n := range []string{"j", "json"} {
			fs.BoolVar(&asJSON, n, false, "")
		}
	}
	if err = fs.Parse(args); err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err == nil && rng != "" {
		_, err = stats.ParseRange(rng, time.Now())
	}
	return
}

func cmdStats(cfg *config.Config, args []string) error {
	serve := len(args) > 0 && args[0] == "serve"
	if serve {
		args = args[1:]
	}
	rng, asJSON, host, port, err := statsFlags(args, serve)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(statsUsage)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, statsUsage)
	}
	if serve {
		return serveStats(cfg, rng, host, port)
	}
	recs, err := stats.Load(cfg.Dir)
	if err != nil {
		return err
	}
	rep, err := stats.Build(recs, rng, time.Now(), statsPrices(cfg))
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	stats.Summary(os.Stdout, rep, newPalette(os.Stdout).reset != "")
	return nil
}

func serveStats(cfg *config.Config, rng, host string, port int) error {
	explicit := port > 0
	if !explicit {
		port = 3847
	}
	ln, err := stats.Listen(host, port, explicit)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String() + "/"
	if rng != "" {
		url += "?range=" + rng
	}
	fmt.Printf("Dashboard at %s\nPress Ctrl+C to stop.\n", url)
	openURL(url)
	d := &stats.Dashboard{Dir: cfg.Dir, Overrides: cfg.Prices}
	if cached, at := stats.CachedPrices(cfg.Dir); cached != nil {
		d.SetCatalog(cached, at, false)
	}
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { // fresh prices for every serve; the page uses the cache or built-in table meanwhile
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		prices, at, err := stats.FetchPrices(fctx, cfg.Dir)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Printf("Couldn't fetch prices from models.dev (%v); using %s.\n", err, d.PriceSource())
			}
			return
		}
		d.SetCatalog(prices, at, true)
		// Parsing the 5+ MB catalog is a one-off; hand that memory back
		// instead of holding it for as long as the dashboard runs.
		debug.FreeOSMemory()
		fmt.Printf("Prices for %d models fetched from models.dev.\n", len(prices))
	}()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// statsCommand is /stats: the summary for the last 7 days.
func (a *app) statsCommand(args string) {
	rng := args
	if rng == "" {
		rng = "7d"
	}
	recs, err := stats.Load(a.cfg.Dir)
	if err != nil {
		a.ui.Errorf("stats: %v", err)
		return
	}
	rep, err := stats.Build(recs, rng, time.Now(), statsPrices(a.cfg))
	if err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.ui.Finish()
	stats.Summary(a.out, rep, a.ui.c.reset != "")
	a.ui.Dim("tego stats serve opens the dashboard · /stats 24h|7d|30d|90d|all")
}

func openURL(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// statsPrices are config overrides over the models.dev prices that tego
// stats serve last fetched; the summary itself never goes online.
func statsPrices(cfg *config.Config) stats.Prices {
	catalog, _ := stats.CachedPrices(cfg.Dir)
	return stats.Prices{Overrides: cfg.Prices, Catalog: catalog}
}
