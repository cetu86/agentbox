package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const planUsage = `Usage: agentbox plan <subcommand> [flags] [args]

  new "goal"            start a planner agent that writes .agentbox/plan.json; also
                        written as: agentbox plan "goal with several words"
  collect               validate the planner's plan and save it (after it reported done)
  check                 validate .agentbox/plan.json and show the parallel groups
  status                show every task, its state and the latest note
  run [--once]          start ready tasks, collect finished ones, keep going
                        until the plan is complete (Ctrl-C stops; state is kept)
  approve <id>          sign off a finished task and merge it into plan/<name>
  reject  <id> [-m MSG] send a finished task back with feedback (without -m: block it)
  done    <id>          mark a task done when its agent exited without the marker
  retry   <id>          discard a blocked or stuck task's box and start it again
  finish                merge the completed plan/<name> into the current branch

Attach to a task's agent with: agentbox attach <id>
`

func cmdPlan(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, planUsage)
		if len(args) == 0 {
			os.Exit(2)
		}
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return cmdPlanNew(rest)
	case "collect":
		return cmdPlanCollect(rest)
	case "check":
		return cmdPlanCheck(rest)
	case "status":
		return cmdPlanStatus(rest)
	case "run":
		return cmdPlanRun(rest)
	case "approve":
		return cmdPlanApprove(rest)
	case "reject":
		return cmdPlanReject(rest)
	case "done":
		return cmdPlanDone(rest)
	case "retry":
		return cmdPlanRetry(rest)
	case "finish":
		return cmdPlanFinish(rest)
	}
	if strings.ContainsAny(sub, " \t") {
		return cmdPlanNew(args)
	}
	fmt.Fprint(os.Stderr, planUsage)
	return fmt.Errorf("unknown plan subcommand %q", sub)
}

func cmdPlanNew(args []string) error {
	cfg, rest, err := parse("plan new", args, nil)
	if err != nil {
		return err
	}
	goal := strings.TrimSpace(strings.Join(rest, " "))
	if err := cfg.startPlanner(goal); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Planner started. Talk it through; when the plan is ready it writes its marker.")
	fmt.Fprintln(os.Stderr, "Detach at any time; collect later with: agentbox plan collect")
	if err := cfg.attachContainer(plannerBox); err != nil {
		return err
	}
	return collectAndReport(cfg)
}

func cmdPlanCollect(args []string) error {
	cfg, _, err := parse("plan collect", args, nil)
	if err != nil {
		return err
	}
	return collectAndReport(cfg)
}

func collectAndReport(cfg *Config) error {
	r, err := cfg.collectPlan()
	if err != nil {
		return err
	}
	switch r.Kind {
	case plannerMissing:
		return errors.New("no planner box; start one with: agentbox plan new \"goal\"")
	case plannerWorking:
		fmt.Println("The planner has not reported done yet. Attach with: agentbox attach " + plannerBox)
	case plannerBlocked:
		fmt.Printf("The planner gave up: %s\nDiscard it with: agentbox discard %s\n", r.Message, plannerBox)
	case plannerInvalid:
		fmt.Printf("The plan is invalid and was sent back to the planner:\n%s\nAttach with: agentbox attach %s, then run: agentbox plan collect\n", r.Message, plannerBox)
	case plannerCollected:
		fmt.Printf("Plan %q saved: %d tasks.\n", r.Plan.Name, len(r.Plan.Tasks))
		if r.Message != "" {
			fmt.Println(r.Message)
		}
		fmt.Println("Review it in the TUI (agentbox, then P) or run: agentbox plan check")
	}
	return nil
}

func cmdPlanCheck(args []string) error {
	cfg, _, err := parse("plan check", args, nil)
	if err != nil {
		return err
	}
	p, err := loadPlan(cfg.Project)
	if err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	fmt.Printf("plan %q: %d tasks, integration branch %s\n", p.Name, len(p.Tasks), planBranch(p.Name))
	for i, level := range p.levels() {
		fmt.Printf("  group %d (parallel):\n", i+1)
		for _, id := range level {
			t := p.task(id)
			line := "    " + taskLabel(t)
			if len(t.DependsOn) > 0 {
				line += "  after " + strings.Join(t.DependsOn, ", ")
			}
			if t.AutoApprove {
				line += "  [auto-approve]"
			}
			fmt.Println(line)
		}
	}
	fmt.Printf("max %d agents at once (max_parallel)\n", effectiveMaxParallel(cfg))
	return nil
}

func effectiveMaxParallel(c *Config) int {
	if c.MaxParallel > 0 {
		return c.MaxParallel
	}
	return defaultMaxParallel
}

func cmdPlanStatus(args []string) error {
	cfg, _, err := parse("plan status", args, nil)
	if err != nil {
		return err
	}
	return cfg.withPlan(func(o *orchestrator) error {
		printPlanStatus(o)
		return nil
	})
}

func printPlanStatus(o *orchestrator) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSTATUS\tAFTER\tNOTE")
	for _, t := range o.plan.Tasks {
		note := ""
		if ts := o.state.Tasks[t.ID]; ts != nil {
			note = firstLine(ts.Note)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, o.plan.displayStatus(o.state, t.ID), strings.Join(t.DependsOn, ","), note)
	}
	w.Flush()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 100 {
		line = line[:100] + "…"
	}
	return line
}

func cmdPlanRun(args []string) error {
	var once bool
	var interval time.Duration
	cfg, _, err := parse("plan run", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&once, "once", false, "do one pass and exit")
		fs.DurationVar(&interval, "interval", 5*time.Second, "time between passes")
	})
	if err != nil {
		return err
	}
	if interval < time.Second {
		interval = time.Second
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	lastProblem := ""
	for {
		var finished, idle, reviewing bool
		var events []string
		stepErr := cfg.withPlan(func(o *orchestrator) error {
			err := o.step()
			events = o.events
			finished = o.plan.isComplete(o.state)
			idle = o.idle()
			for _, t := range o.plan.Tasks {
				if o.state.status(t.ID) == taskReview {
					reviewing = true
				}
			}
			return err
		})
		for _, e := range events {
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), e)
		}
		if stepErr != nil {
			if once {
				return stepErr
			}
			if msg := stepErr.Error(); msg != lastProblem {
				fmt.Fprintln(os.Stderr, "agentbox: plan:", msg)
				lastProblem = msg
			}
		} else {
			lastProblem = ""
		}
		switch {
		case finished:
			fmt.Printf("plan complete. Review the result and merge it with: agentbox plan finish\n")
			return nil
		case once:
			return nil
		case stepErr == nil && idle && !reviewing:
			fmt.Println("nothing left to run; remaining tasks are blocked and need your attention (agentbox plan status)")
			return nil
		}
		select {
		case <-stop:
			fmt.Println("stopped; plan state is saved")
			return nil
		case <-time.After(interval):
		}
	}
}

// planTaskCommand runs fn on the orchestrator for the single task named in the
// arguments and reports the resulting events.
func planTaskCommand(name string, args []string, extra func(*flag.FlagSet), fn func(o *orchestrator, id string) error) error {
	cfg, rest, err := parse("plan "+name, args, extra)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: agentbox plan %s <task-id>", name)
	}
	return cfg.withPlan(func(o *orchestrator) error {
		err := fn(o, rest[0])
		for _, e := range o.events {
			fmt.Println(e)
		}
		return err
	})
}

func cmdPlanApprove(args []string) error {
	return planTaskCommand("approve", args, nil, func(o *orchestrator, id string) error { return o.approve(id) })
}

func cmdPlanReject(args []string) error {
	var feedback string
	return planTaskCommand("reject", args, func(fs *flag.FlagSet) {
		fs.StringVar(&feedback, "m", "", "feedback for the agent; it is restarted with it")
	}, func(o *orchestrator, id string) error { return o.reject(id, strings.TrimSpace(feedback)) })
}

func cmdPlanDone(args []string) error {
	return planTaskCommand("done", args, nil, func(o *orchestrator, id string) error { return o.markDone(id) })
}

func cmdPlanRetry(args []string) error {
	return planTaskCommand("retry", args, nil, func(o *orchestrator, id string) error { return o.retry(id) })
}

func cmdPlanFinish(args []string) error {
	cfg, _, err := parse("plan finish", args, nil)
	if err != nil {
		return err
	}
	return cfg.withPlan(func(o *orchestrator) error {
		if !o.plan.isComplete(o.state) {
			printPlanStatus(o)
			return fmt.Errorf("plan %q is not complete: every task must be accepted first", o.plan.Name)
		}
		msg, err := cfg.finishPlan(o.plan.Name)
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	})
}
