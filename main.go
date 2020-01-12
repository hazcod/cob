package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/xerrors"

	"github.com/olekukonko/tablewriter"
	"github.com/urfave/cli/v2"
	"golang.org/x/tools/benchmark/parse"
	"gopkg.in/src-d/go-git.v4"
)

type result struct {
	Name                   string
	RatioNsPerOp           float64
	RatioAllocedBytesPerOp float64
}

func main() {
	app := &cli.App{
		Name:  "cob",
		Usage: "Continuous Benchmark for Go project",
		Action: func(c *cli.Context) error {
			return run(newConfig(c))
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "only-degression",
				Usage: "Show only benchmarks with worse score",
			},
			&cli.Float64Flag{
				Name:  "threshold",
				Usage: "The program fails if the benchmark gets worse than the threshold",
				Value: 0.1,
			},
			&cli.StringFlag{
				Name:  "bench",
				Usage: "Run only those benchmarks matching a regular expression.",
				Value: ".",
			},
			&cli.BoolFlag{
				Name:  "benchmem",
				Usage: "Print memory allocation statistics for benchmarks.",
			},
			&cli.StringFlag{
				Name:  "benchtime",
				Usage: "Run enough iterations of each benchmark to take t, specified as a time.Duration (for example, -benchtime 1h30s).",
				Value: "1s",
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func run(c config) error {
	r, err := git.PlainOpen(".")
	if err != nil {
		return xerrors.Errorf("unable to open the git repository: %w", err)
	}

	head, err := r.Head()
	if err != nil {
		return xerrors.Errorf("unable to get the reference where HEAD is pointing to: %w", err)
	}

	prev, err := r.ResolveRevision("HEAD~1")
	if err != nil {
		return xerrors.Errorf("unable to resolves revision to corresponding hash: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return xerrors.Errorf("unable to get a worktree based on the given fs: %w", err)
	}

	err = w.Reset(&git.ResetOptions{Commit: *prev, Mode: git.HardReset})
	if err != nil {
		return xerrors.Errorf("failed to reset the worktree to a previous commit: %w", err)
	}

	args := prepareBenchArgs(c)

	log.Printf("Run Benchmark: %s %s", prev, "HEAD{@1}")
	prevSet, err := runBenchmark(args)
	if err != nil {
		return xerrors.Errorf("failed to run a benchmark: %w", err)
	}

	err = w.Reset(&git.ResetOptions{Commit: head.Hash(), Mode: git.HardReset})
	if err != nil {
		return xerrors.Errorf("failed to reset the worktree to HEAD: %w", err)
	}

	log.Printf("Run Benchmark: %s %s", head.Hash(), "HEAD")
	headSet, err := runBenchmark(args)
	if err != nil {
		return xerrors.Errorf("failed to run a benchmark: %w", err)
	}

	var ratios []result
	var rows [][]string
	for benchName, headBenchmarks := range headSet {
		prevBenchmarks, ok := prevSet[benchName]
		if !ok {
			continue
		}
		if len(headBenchmarks) == 0 || len(prevBenchmarks) == 0 {
			continue
		}
		prevBench := prevBenchmarks[0]
		headBench := headBenchmarks[0]

		var ratioNsPerOp float64
		if prevBench.NsPerOp != 0 {
			ratioNsPerOp = (headBench.NsPerOp - prevBench.NsPerOp) / prevBench.NsPerOp
		}

		var ratioAllocedBytesPerOp float64
		if prevBench.AllocedBytesPerOp != 0 {
			ratioAllocedBytesPerOp = float64(headBench.AllocedBytesPerOp-prevBench.AllocedBytesPerOp) / float64(prevBench.AllocedBytesPerOp)
		}

		rows = append(rows, generateRow("HEAD", headBench, c.benchmem))
		rows = append(rows, generateRow("HEAD@{1}", prevBench, c.benchmem))

		ratios = append(ratios, result{
			Name:                   benchName,
			RatioNsPerOp:           ratioNsPerOp,
			RatioAllocedBytesPerOp: ratioAllocedBytesPerOp,
		})
	}

	showResult(rows, c.benchmem)
	degression := showRatio(ratios, c.benchmem, c.threshold, c.onlyDegression)
	if degression {
		return xerrors.New("This commit makes benchmarks worse")
	}

	return nil
}

func prepareBenchArgs(c config) []string {
	args := []string{"test", "-benchtime", c.benchtime, "-bench", c.bench}
	if c.benchmem {
		args = append(args, "-benchmem")
	}
	args = append(args, c.args...)
	return args
}

func runBenchmark(args []string) (parse.Set, error) {
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, xerrors.Errorf("failed to run 'go test' command: %w", err)
	}

	b := bytes.NewBuffer(out)
	s, err := parse.ParseSet(b)
	if err != nil {
		return nil, xerrors.Errorf("failed to parse a result of benchmarks: %w", err)
	}
	return s, nil
}

func generateRow(ref string, b *parse.Benchmark, benchmem bool) []string {
	row := []string{b.Name, ref, fmt.Sprintf(" %.2f ns/op", b.NsPerOp)}
	if benchmem {
		row = append(row, fmt.Sprintf(" %d B/op", b.AllocedBytesPerOp))
	}
	return row
}

func showResult(rows [][]string, benchmem bool) {
	fmt.Println("\nResult")
	fmt.Println(strings.Repeat("=", 6), "\n")

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoFormatHeaders(false)
	table.SetAlignment(tablewriter.ALIGN_CENTER)
	headers := []string{"Name", "Commit", "NsPerOp"}
	if benchmem {
		headers = append(headers, "AllocedBytesPerOp")
	}
	table.SetHeader(headers)
	table.SetAutoMergeCells(true)
	table.SetRowLine(true)
	table.AppendBulk(rows)
	table.Render()
}

func showRatio(results []result, benchmem bool, threshold float64, onlyDegression bool) bool {
	fmt.Println("\nComparison")
	fmt.Println(strings.Repeat("=", 10), "\n")

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoFormatHeaders(false)
	table.SetAlignment(tablewriter.ALIGN_CENTER)
	table.SetRowLine(true)
	headers := []string{"Name", "NsPerOp"}
	if benchmem {
		headers = append(headers, "AllocedBytesPerOp")
	}
	table.SetHeader(headers)

	var degression bool
	for _, result := range results {
		if onlyDegression &&
			(result.RatioNsPerOp <= threshold && result.RatioAllocedBytesPerOp <= threshold) {
			continue
		}
		degression = true
		row := []string{result.Name, generateRatioItem(result.RatioNsPerOp)}
		if benchmem {
			row = append(row, generateRatioItem(result.RatioAllocedBytesPerOp))
		}

		colors := []tablewriter.Colors{{}}
		colors = append(colors, generateColor(result.RatioNsPerOp))
		colors = append(colors, generateColor(result.RatioAllocedBytesPerOp))
		table.Rich(row, colors)
	}
	table.Render()
	fmt.Println()
	return degression
}

func generateRatioItem(ratio float64) string {
	if -0.0001 < ratio && ratio < 0.0001 {
		ratio = 0
	}
	if 0 <= ratio {
		return fmt.Sprintf("%.2f%%", 100*ratio)
	}
	return fmt.Sprintf("%.2f%%", -100*ratio)
}

func generateColor(ratio float64) tablewriter.Colors {
	if ratio > 0 {
		return tablewriter.Colors{tablewriter.Bold, tablewriter.FgHiRedColor}
	}
	return tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlueColor}
}
