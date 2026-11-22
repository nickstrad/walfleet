package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/nickstrad/walfleet/internal/repocheck"
)

// labEnv is set inside the lab container (compose.yaml, Dockerfile.lab). The
// lab image carries the pinned DuckDB CLI, so "course run" does its work there.
const labEnv = "COURSE_LAB"

var experimentIDPattern = regexp.MustCompile(`^\d+\.\d+$`)

// runRun implements "course run [-i] [-update] <N.M>": run one experiment,
// then present its CSV tables with the DuckDB script report.sql next to its main.go.
func runRun(ctx context.Context, root string, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	interactive := flags.Bool("i", false, "stay in the DuckDB shell after the report")
	update := flags.Bool("update", false, "store the tables and the rendered report as the experiment's evidence")
	if err := flags.Parse(args); err != nil {
		return exitError{2}
	}
	if flags.NArg() != 1 || !experimentIDPattern.MatchString(flags.Arg(0)) {
		return usageError("run needs one experiment number such as 3.1")
	}
	if os.Getenv(labEnv) == "" {
		return runInLab(ctx, root, args)
	}
	experiment, err := findExperiment(root, flags.Arg(0))
	if err != nil {
		return err
	}
	stdout, runErr := runExperiment(ctx, root, experiment)
	if runErr != nil || !experiment.HasReport() {
		// A failed run, or an experiment without a report: show its own output.
		fmt.Print(stdout)
		return runErr
	}
	results := experiment.ResultsDir(root)
	verdict := repocheck.VerdictLine(stdout)
	if *update {
		if err := repocheck.UpdateEvidence(ctx, experiment, results, verdict); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "evidence updated: %s and %s/\n", experiment.Evidence, experiment.EvidenceDir())
	}
	duckdb, err := repocheck.DuckDB(ctx)
	if err != nil {
		return fmt.Errorf("%w; rebuild the lab image: docker compose build lab", err)
	}
	info := experiment.InfoReport()
	if _, err := os.Stat(info); err != nil {
		info = ""
	}
	script, err := repocheck.WriteWalkthrough(results, experiment.Report(), info, verdict, *interactive)
	if err != nil {
		return err
	}
	mode := "-f"
	if *interactive {
		mode = "-init"
	}
	cmd := exec.CommandContext(ctx, duckdb, "-no-agent", mode, script)
	cmd.Dir = results
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

// runInLab re-runs the same command inside the lab container.
func runInLab(ctx context.Context, root string, args []string) error {
	command := append([]string{"compose", "run", "--rm", "lab", "go", "run", "./cmd/course", "run"}, args...)
	cmd := exec.CommandContext(ctx, "docker", command...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

func exitCode(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exitError{exit.ExitCode()}
	}
	return err
}

func findExperiment(root, id string) (repocheck.Experiment, error) {
	chapters, err := repocheck.Chapters(root, "")
	if err != nil {
		return repocheck.Experiment{}, err
	}
	for _, chapter := range chapters {
		experiments, err := repocheck.Experiments(chapter)
		if err != nil {
			return repocheck.Experiment{}, err
		}
		for _, experiment := range experiments {
			if strings.HasPrefix(experiment.Name, id+"-") {
				return experiment, nil
			}
		}
	}
	return repocheck.Experiment{}, fmt.Errorf("no experiment %s", id)
}

// runExperiment runs the experiment from root and returns its stdout. The
// error is non-nil when the experiment exits non-zero.
func runExperiment(ctx context.Context, root string, experiment repocheck.Experiment) (string, error) {
	fmt.Fprintf(os.Stderr, "running %s ...\n", experiment.Package)
	cmd := exec.CommandContext(ctx, "go", "run", experiment.Package)
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), exitCode(err)
}
