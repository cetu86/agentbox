package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New("dummy agent expects one task")
	}
	time.Sleep(2 * time.Second)
	task := args[0]
	run := 1
	if data, err := os.ReadFile("agent-runs"); err == nil {
		if _, scanErr := fmt.Sscanf(string(data), "%d", &run); scanErr != nil {
			return fmt.Errorf("read previous dummy run count: %w", scanErr)
		}
		run++
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile("agent-runs", []byte(fmt.Sprintf("%d\n", run)), 0o644); err != nil {
		return err
	}
	fmt.Printf("DUMMY_AGENT_RUN=%d TASK=%s\n", run, task)
	switch task {
	case "interactive":
		fmt.Println("DUMMY_AGENT_READY: should this change be approved?")
		reader := bufio.NewReader(os.Stdin)
		answer, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		answer = strings.TrimSuffix(strings.TrimSuffix(answer, "\n"), "\r")
		fmt.Printf("DUMMY_AGENT_ANSWER=%s\n", answer)
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		fmt.Println("DUMMY_AGENT_CONTINUING")
		return os.WriteFile("agent-result.txt", []byte("approved: "+answer+"\n"), 0o644)
	case "finish":
		fmt.Println("DUMMY_AGENT_FINISHED")
		if err := os.WriteFile("agent-result.txt", []byte(fmt.Sprintf("completed run %d\n", run)), 0o644); err != nil {
			return err
		}
		time.Sleep(time.Second)
		return nil
	default:
		return fmt.Errorf("unexpected dummy task %q", task)
	}
}
