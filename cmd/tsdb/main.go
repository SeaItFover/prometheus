package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
	"unsafe"

	"github.com/fabxc/tsdb"
	"github.com/fabxc/tsdb/labels"
	promlabels "github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/textparse"
	"github.com/spf13/cobra"
)

func main() {
	// Start HTTP server for pprof endpoint.
	go http.ListenAndServe(":9999", nil)

	root := &cobra.Command{
		Use:   "tsdb",
		Short: "CLI tool for tsdb",
	}

	root.AddCommand(
		NewBenchCommand(),
	)

	flag.CommandLine.Set("log.level", "debug")

	root.Execute()
}

func NewBenchCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "bench",
		Short: "run benchmarks",
	}
	c.AddCommand(NewBenchWriteCommand())

	return c
}

type writeBenchmark struct {
	outPath    string
	cleanup    bool
	numMetrics int

	storage *tsdb.DB

	cpuprof   *os.File
	memprof   *os.File
	blockprof *os.File
}

func NewBenchWriteCommand() *cobra.Command {
	var wb writeBenchmark
	c := &cobra.Command{
		Use:   "write <file>",
		Short: "run a write performance benchmark",
		Run:   wb.run,
	}
	c.PersistentFlags().StringVar(&wb.outPath, "out", "benchout/", "set the output path")
	c.PersistentFlags().IntVar(&wb.numMetrics, "metrics", 10000, "number of metrics to read")
	return c
}

func (b *writeBenchmark) run(cmd *cobra.Command, args []string) {
	if len(args) != 1 {
		exitWithError(fmt.Errorf("missing file argument"))
	}
	if b.outPath == "" {
		dir, err := ioutil.TempDir("", "tsdb_bench")
		if err != nil {
			exitWithError(err)
		}
		b.outPath = dir
		b.cleanup = true
	}
	if err := os.RemoveAll(b.outPath); err != nil {
		exitWithError(err)
	}
	if err := os.MkdirAll(b.outPath, 0777); err != nil {
		exitWithError(err)
	}

	dir := filepath.Join(b.outPath, "storage")

	st, err := tsdb.Open(dir, nil, nil, &tsdb.Options{
		WALFlushInterval:  200 * time.Millisecond,
		RetentionDuration: 2 * 24 * 60 * 60 * 1000, // 1 days in milliseconds
		MinBlockDuration:  3 * 60 * 60 * 1000,      // 2 hours in milliseconds
		MaxBlockDuration:  27 * 60 * 60 * 1000,     // 1 days in milliseconds
		AppendableBlocks:  2,
	})
	if err != nil {
		exitWithError(err)
	}
	b.storage = st

	var metrics []labels.Labels

	measureTime("readData", func() {
		f, err := os.Open(args[0])
		if err != nil {
			exitWithError(err)
		}
		defer f.Close()

		metrics, err = readPrometheusLabels(f, b.numMetrics)
		if err != nil {
			exitWithError(err)
		}
	})

	defer func() {
		reportSize(dir)
		if b.cleanup {
			os.RemoveAll(b.outPath)
		}
	}()

	var total uint64

	dur := measureTime("ingestScrapes", func() {
		b.startProfiling()
		total, err = b.ingestScrapes(metrics, 3000)
		if err != nil {
			exitWithError(err)
		}
	})

	fmt.Println(" > total samples:", total)
	fmt.Println(" > samples/sec:", float64(total)/dur.Seconds())

	measureTime("stopStorage", func() {
		if err := b.storage.Close(); err != nil {
			exitWithError(err)
		}
		b.stopProfiling()
	})
}

func (b *writeBenchmark) ingestScrapes(lbls []labels.Labels, scrapeCount int) (uint64, error) {
	var mu sync.Mutex
	var total uint64

	for i := 0; i < scrapeCount; i += 100 {
		var wg sync.WaitGroup
		lbls := lbls
		for len(lbls) > 0 {
			l := 1000
			if len(lbls) < 1000 {
				l = len(lbls)
			}
			batch := lbls[:l]
			lbls = lbls[l:]

			wg.Add(1)
			go func() {
				n, err := b.ingestScrapesShard(batch, 100, int64(30000*i))
				if err != nil {
					// exitWithError(err)
					fmt.Println(" err", err)
				}
				mu.Lock()
				total += n
				mu.Unlock()
				wg.Done()
			}()
		}
		wg.Wait()
	}

	return total, nil
}

func (b *writeBenchmark) ingestScrapesShard(metrics []labels.Labels, scrapeCount int, baset int64) (uint64, error) {
	ts := baset

	type sample struct {
		labels labels.Labels
		value  int64
		ref    *uint64
	}

	scrape := make([]*sample, 0, len(metrics))

	for _, m := range metrics {
		scrape = append(scrape, &sample{
			labels: m,
			value:  123456789,
		})
	}
	total := uint64(0)

	for i := 0; i < scrapeCount; i++ {
		app := b.storage.Appender()
		ts += int64(30000)

		for _, s := range scrape {
			s.value += 1000

			if s.ref == nil {
				ref, err := app.Add(s.labels, ts, float64(s.value))
				if err != nil {
					panic(err)
				}
				s.ref = &ref
			} else if err := app.AddFast(*s.ref, ts, float64(s.value)); err != nil {

				if err.Error() != "not found" {
					panic(err)
				}

				ref, err := app.Add(s.labels, ts, float64(s.value))
				if err != nil {
					panic(err)
				}
				s.ref = &ref
			}

			total++
		}
		if err := app.Commit(); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (b *writeBenchmark) startProfiling() {
	var err error

	// Start CPU profiling.
	b.cpuprof, err = os.Create(filepath.Join(b.outPath, "cpu.prof"))
	if err != nil {
		exitWithError(fmt.Errorf("bench: could not create cpu profile: %v\n", err))
	}
	pprof.StartCPUProfile(b.cpuprof)

	// Start memory profiling.
	b.memprof, err = os.Create(filepath.Join(b.outPath, "mem.prof"))
	if err != nil {
		exitWithError(fmt.Errorf("bench: could not create memory profile: %v\n", err))
	}
	runtime.MemProfileRate = 4096

	// Start fatal profiling.
	b.blockprof, err = os.Create(filepath.Join(b.outPath, "block.prof"))
	if err != nil {
		exitWithError(fmt.Errorf("bench: could not create block profile: %v\n", err))
	}
	runtime.SetBlockProfileRate(1)
}

func (b *writeBenchmark) stopProfiling() {
	if b.cpuprof != nil {
		pprof.StopCPUProfile()
		b.cpuprof.Close()
		b.cpuprof = nil
	}
	if b.memprof != nil {
		pprof.Lookup("heap").WriteTo(b.memprof, 0)
		b.memprof.Close()
		b.memprof = nil
	}
	if b.blockprof != nil {
		pprof.Lookup("block").WriteTo(b.blockprof, 0)
		b.blockprof.Close()
		b.blockprof = nil
		runtime.SetBlockProfileRate(0)
	}
}

func reportSize(dir string) {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == dir {
			return err
		}
		if info.Size() < 10*1024*1024 {
			return nil
		}
		fmt.Printf(" > file=%s size=%.04fGiB\n", path[len(dir):], float64(info.Size())/1024/1024/1024)
		return nil
	})
	if err != nil {
		exitWithError(err)
	}
}

func measureTime(stage string, f func()) time.Duration {
	fmt.Printf(">> start stage=%s\n", stage)
	start := time.Now()
	f()
	fmt.Printf(">> completed stage=%s duration=%s\n", stage, time.Since(start))
	return time.Since(start)
}

func readPrometheusLabels(r io.Reader, n int) ([]labels.Labels, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	p := textparse.New(b)
	i := 0
	var mets []labels.Labels
	hashes := map[uint64]struct{}{}

	for p.Next() && i < n {
		m := make(labels.Labels, 0, 10)
		p.Metric((*promlabels.Labels)(unsafe.Pointer(&m)))

		h := m.Hash()
		if _, ok := hashes[h]; ok {
			continue
		}
		mets = append(mets, m)
		hashes[h] = struct{}{}
		i++
	}
	return mets, p.Err()
}

func exitWithError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
