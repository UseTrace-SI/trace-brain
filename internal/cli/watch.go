package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type watcherIdentity struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func watchCmd() *cobra.Command {
	var interval int
	var agentCmd, traceDB string
	var daemon bool
	cmd := &cobra.Command{Use: "watch", Short: "Periodically extract new or changed memories", RunE: func(cmd *cobra.Command, args []string) error {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
			return err
		}
		if interval < 1 {
			interval = 60
		}
		if daemon {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			log, err := os.OpenFile(dbPath+".watch.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer log.Close()
			child := exec.Command(exe, "watch", "--interval", fmt.Sprint(interval), "--agent", agentCmd, "--trace-db", traceDB, "--db", dbPath)
			child.Stdout, child.Stderr = log, log
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := child.Start(); err != nil {
				return err
			}
			pid := child.Process.Pid
			defer child.Process.Release()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				b, err := os.ReadFile(dbPath + ".watch.pid")
				var identity watcherIdentity
				if err == nil && json.Unmarshal(b, &identity) == nil && identity.PID == pid {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "watcher started (pid %d)\n", pid)
					return err
				}
				if child.Process.Signal(syscall.Signal(0)) != nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			return fmt.Errorf("watcher did not become ready; inspect %s.watch.log", dbPath)
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runWatcher(ctx, dbPath, time.Duration(interval)*time.Second, func(ctx context.Context) error {
			return importTrace(ctx, cmd.OutOrStdout(), traceDB, agentCmd, true)
		})
	}}
	cmd.Flags().IntVar(&interval, "interval", 60, "seconds between runs")
	cmd.Flags().StringVar(&agentCmd, "agent", "", "agent command")
	cmd.Flags().StringVar(&traceDB, "trace-db", ".trace/trace.db", "path to Trace database")
	cmd.Flags().BoolVar(&daemon, "daemon", false, "run in background")
	return cmd
}

func runWatcher(parent context.Context, path string, interval time.Duration, extract func(context.Context) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".watch.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("watcher already running: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	for _, suffix := range []string{".watch.stop", ".watch.ack"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	identity := watcherIdentity{PID: os.Getpid(), Token: hex.EncodeToString(token[:])}
	b, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	pidPath := path + ".watch.pid"
	f, err := os.CreateTemp(filepath.Dir(path), ".watch-pid-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), pidPath); err != nil {
		return err
	}
	defer os.Remove(pidPath)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		poll := time.NewTicker(20 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
			}
			data, err := os.ReadFile(path + ".watch.stop")
			if err == nil && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(data))), []byte(identity.Token)) == 1 {
				// Acknowledge only a request for this instance, never a stale PID.
				if os.WriteFile(path+".watch.ack", []byte(identity.Token), 0600) == nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done; _ = os.Remove(path + ".watch.stop") }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := extract(ctx); err != nil && ctx.Err() == nil {
			return fmt.Errorf("watch extraction: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func stopWatcher(path string) error {
	b, err := os.ReadFile(path + ".watch.pid")
	if err != nil {
		return fmt.Errorf("reading watcher identity: %w", err)
	}
	var identity watcherIdentity
	if json.Unmarshal(b, &identity) != nil || identity.PID <= 0 || len(identity.Token) != 64 {
		return fmt.Errorf("invalid or legacy watcher identity; no process was signaled")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".watch-stop-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(identity.Token); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path+".watch.stop"); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path + ".watch.ack")
		if err == nil && subtle.ConstantTimeCompare(data, []byte(identity.Token)) == 1 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("watcher did not authenticate the stop request; no process was signaled")
}

func stopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Gracefully stop the authenticated local watcher", RunE: func(cmd *cobra.Command, args []string) error {
		if err := stopWatcher(dbPath); err != nil {
			return err
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "watcher stopping")
		return err
	}}
}
