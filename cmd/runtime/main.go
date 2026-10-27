package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/agent"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	dockersandbox "github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
func execute(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "AgentEval Runtime v"+runner.Version+"\nCommands: version, doctor, prepare, hash, run, get, serve, submit, status, list, cancel, events, tasks, batch, experiment, reeval, import, dataset, export\nUse <command> --help for flags.")
		return 0
	}
	if args[0] == "version" {
		fmt.Fprintln(out, "AgentEval Runtime v"+runner.Version)
		return 0
	}
	switch args[0] {
	case "dataset":
        return executeDataset(ctx, args[1:], out, errout)
    case "export":
        return executeExport(ctx, args[1:], out, errout)
    case "import":
		return executeImport(ctx, args[1:], out, errout)
	case "experiment":
		return executeExperiment(ctx, args[1:], out, errout)
	case "reeval":
		return executeReevaluation(ctx, args[1:], out, errout)
	case "submit", "status", "list", "cancel", "events", "tasks", "batch":
		return executeQueue(ctx, args, out, errout)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(errout)
	confFile := fs.String("config", "", "local JSON configuration (optional)")
	project := fs.String("project", ".", "trusted project root")
	var taskFile, agentName, scriptName, artifactDir, model, hashDir *string
	var listen *string
	var workers, queueCapacity *int
	switch args[0] {
	case "serve":
		listen = fs.String("listen", "", "loopback host:port override")
		workers = fs.Int("workers", 0, "worker count override (1..8)")
		queueCapacity = fs.Int("queue-capacity", 0, "queued job limit override (1..1000)")
		artifactDir = fs.String("artifacts", "", "artifact directory override")
	case "doctor", "prepare":
	case "hash":
		hashDir = fs.String("dir", "", "bundle directory")
	case "run":
		taskFile = fs.String("task", "tasks/fixtures/go-parser-001/task.json", "trusted task JSON")
		agentName = fs.String("agent", "scripted", "scripted or llm")
		scriptName = fs.String("script", "success", "scripted fixture action sequence")
		artifactDir = fs.String("artifacts", "", "artifact directory override")
		model = fs.String("model", "", "OpenAI model override")
	case "get":
		artifactDir = fs.String("artifacts", "", "artifact directory override")
	default:
		fmt.Fprintf(errout, "unknown command %q\n", args[0])
		return 1
	}
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if args[0] == "hash" {
		if *hashDir == "" || fs.NArg() != 0 {
			fmt.Fprintln(errout, "hash requires --dir")
			return 1
		}
		files, err := task.ReadBundle(*hashDir)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintln(out, task.HashFiles(files))
		return 0
	}
	c, err := config.Load(*confFile)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if artifactDir != nil && *artifactDir != "" {
		c.ArtifactsDir = *artifactDir
	}
	if args[0] == "get" {
		if fs.NArg() != 1 || !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(fs.Arg(0)) {
			fmt.Fprintln(errout, "get requires a 24-character run ID; flags precede the ID")
			return 1
		}
		f, err := os.Open(filepath.Join(c.ArtifactsDir, fs.Arg(0), "result.json"))
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		defer f.Close()
		data, readErr := io.ReadAll(io.LimitReader(f, 8<<20+1))
		if readErr != nil || len(data) > 8<<20 {
			fmt.Fprintln(errout, "result cannot be read completely within the 8 MiB limit")
			return 1
		}
		if !json.Valid(data) {
			fmt.Fprintln(errout, "result JSON is corrupt")
			return 1
		}
		if _, err = out.Write(data); err != nil {
			return 1
		}
		return 0
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "unexpected positional arguments")
		return 1
	}
	if args[0] == "serve" {
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "listen":
				c.Server.Listen = *listen
			case "workers":
				c.Server.Workers = *workers
			case "queue-capacity":
				c.Server.QueueCapacity = *queueCapacity
			}
		})
		return serveRuntime(ctx, c, *project, out, errout)
	}
	var loaded domain.Task
	var selected domain.Agent
	var publicAgentConfig any
	if args[0] == "run" {
		loaded, err = task.Load(*taskFile, *project)
		if err != nil {
			fmt.Fprintln(errout, "task:", err)
			return 1
		}
		switch *agentName {
		case "scripted":
			script, ok := loaded.Scripts[*scriptName]
			if !ok {
				fmt.Fprintln(errout, "unknown scripted sequence:", *scriptName)
				return 1
			}
			selected = agent.Scripted{Script: script}
			publicAgentConfig = map[string]any{"kind": "scripted", "name": *scriptName, "actions": script.Actions}
		case "llm":
			if *model != "" {
				c.LLM.Model = *model
			}
			if c.LLM.Model == "" {
				fmt.Fprintln(errout, "set --model, config llm.model or OPENAI_MODEL")
				return 1
			}
			key := os.Getenv(c.LLM.APIKeyEnv)
			if key == "" {
				fmt.Fprintf(errout, "set API key in %s on the controller (never in the task)\n", c.LLM.APIKeyEnv)
				return 1
			}
			selected = agent.LLM{Config: agent.Config{BaseURL: c.LLM.BaseURL, Model: c.LLM.Model, APIKey: key, MaxRetries: c.LLM.MaxRetries}}
			publicAgentConfig = map[string]any{"kind": "openai-chat-completions", "base_url": c.LLM.BaseURL, "model": c.LLM.Model, "max_retries": c.LLM.MaxRetries}
		default:
			fmt.Fprintln(errout, "agent must be scripted or llm")
			return 1
		}
	}
	connectCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	factory, err := dockersandbox.New(connectCtx, dockersandbox.Options{Host: c.DockerHost, Image: c.Image})
	ccancel()
	if err != nil {
		fmt.Fprintln(errout, "Docker:", err)
		return 1
	}
	defer factory.Close()
	switch args[0] {
	case "doctor":
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		info, err := factory.Doctor(checkCtx)
		ready := err == nil && info.OSType == "linux" && (info.Architecture == "amd64" || info.Architecture == "x86_64") && info.ImageID != ""
		answer := map[string]any{"schema_version": 1, "runtime_version": runner.Version, "ready": ready, "engine": info, "image": c.Image}
		if err != nil {
			answer["error"] = err.Error()
		}
		if info.ImageID == "" {
			answer["next_step"] = "runtime prepare --project ."
		}
		printJSON(out, answer)
		if !ready {
			return 1
		}
		return 0
	case "prepare":
		buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		id, err := factory.EnsureImage(buildCtx, *project, c.Image)
		if err != nil {
			fmt.Fprintln(errout, "prepare:", err)
			return 1
		}
		printJSON(out, map[string]any{"schema_version": 1, "image": c.Image, "image_id": id})
		return 0
	case "run":
		r, runErr := runner.Run(ctx, factory, loaded, selected, runner.Options{Image: c.Image, ArtifactsDir: c.ArtifactsDir, AgentName: *agentName, AgentConfig: publicAgentConfig})
		printJSON(out, r)
		if runErr != nil {
			fmt.Fprintln(errout, "run:", runErr)
		}
		if r.CleanupStatus == "failed" || r.Verdict == "infra_error" || r.RunID == "" {
			return 1
		}
		if r.Verdict != "success" {
			return 2
		}
		if runErr != nil {
			return 1
		}
		return 0
	}
	return 1
}
func printJSON(w io.Writer, v any) { e := json.NewEncoder(w); e.SetIndent("", "  "); _ = e.Encode(v) }
