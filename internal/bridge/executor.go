package bridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// limitedBuf — потокобезопасный буфер с жёстким лимитом: всё сверх max отбрасывается,
// флаг truncated сигнализирует обрезке. Защищает от команд-пожирателей RAM (find /, dd).
type limitedBuf struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newLimitedBuf(max int) *limitedBuf { return &limitedBuf{max: max} }

func (b *limitedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return len(p), nil // «поглощаем» хвост, не аллоцируя
	}
	room := b.max - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuf) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}

// Executor — маршрутизирует команды по нодам: local → bash на ноутбуке,
// remote → SSH на agrobot-prod. Реализует bridge.Runner.
type Executor struct {
	cfg config.BridgeConfig
	ssh *SSHPool
	log *slog.Logger
}

// NewExecutor собирает исполнитель команд (SSH-пул создаётся лениво при первом remote-вызове).
func NewExecutor(cfg config.BridgeConfig, log *slog.Logger) *Executor {
	return &Executor{cfg: cfg, ssh: newSSHPool(cfg.Remote, log), log: log}
}

// Run выполняет команду на указанной ноде с таймаутом и ограничением вывода.
func (e *Executor) Run(ctx context.Context, node, command string, timeout time.Duration, maxOutput int) (*ExecResult, error) {
	if command == "" {
		return nil, fmt.Errorf("пустая команда")
	}
	if i := strings.IndexByte(command, 0); i >= 0 {
		return nil, fmt.Errorf("команда содержит NUL-байт (потенциальная инъекция)")
	}
	if timeout <= 0 {
		timeout = e.cfg.CommandTimeout.Duration
	}
	if maxOutput <= 0 {
		maxOutput = e.cfg.MaxOutputBytes
	}
	var (
		res *ExecResult
		err error
	)
	switch node {
	case NodeLocal:
		res, err = e.runLocal(ctx, command, timeout, maxOutput)
	case NodeRemote:
		res, err = e.runRemote(ctx, command, timeout, maxOutput)
	default:
		return nil, fmt.Errorf("неизвестная нода %q (ожидается %q или %q)", node, NodeLocal, NodeRemote)
	}
	if res != nil {
		e.log.Info("exec", "node", node, "exit", res.ExitCode, "dur", res.Duration.Round(time.Millisecond), "cmd", truncateCmd(command), "trunc", res.Truncated)
	}
	return res, err
}

func truncateCmd(c string) string {
	if len(c) > 200 {
		return c[:200] + "…"
	}
	return c
}

// runLocal — исполнение на ноутбуке: bash -lc, убийство всей группы процессов по таймауту.
func (e *Executor) runLocal(ctx context.Context, command string, timeout time.Duration, maxOutput int) (*ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "bash", "-lc", "--", command)
	// Своя группа процессов: по таймауту убиваем и детей (bash порождает потомков).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 3 * time.Second // добиваем зависшие пайпы (Go 1.20+)

	stdout, stderr := newLimitedBuf(maxOutput), newLimitedBuf(maxOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	runErr := cmd.Run()
	res := &ExecResult{
		Node:     NodeLocal,
		Command:  command,
		Duration: time.Since(start),
	}
	res.Stdout, res.Truncated = stdout.String()
	res.Stderr, _ = stderr.String()

	// Контекст отменён внешне (не таймаутом команды) — это ошибка вызова, а не команды.
	if cctx.Err() == context.Canceled && ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else if cctx.Err() == context.DeadlineExceeded {
			res.ExitCode = -9
			res.Stderr += "\n[archi] таймаут команды (" + timeout.String() + "), процесс-группа убита"
		} else {
			return res, fmt.Errorf("local exec: %w", runErr)
		}
	}
	return res, nil
}

// runRemote — исполнение на agrobot-prod через SSH-пул с таймаутом и буферизацией.
func (e *Executor) runRemote(ctx context.Context, command string, timeout time.Duration, maxOutput int) (*ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := e.ssh.Run(cctx, command, maxOutput)
	if err != nil {
		return nil, err
	}
	res.Node = NodeRemote
	res.Command = command
	if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		res.Stderr += "\n[archi] таймаут удалённой команды (" + timeout.String() + ")"
		res.ExitCode = -9
	}
	return res, nil
}

// probeLocalSystemd — быстрый опрос systemd (не входит в Runner, используется коллекторами).
func probeSystemd(ctx context.Context, services []string) []ServiceState {
	if len(services) == 0 {
		return nil
	}
	out := make([]ServiceState, 0, len(services))
	for _, s := range services {
		st := ServiceState{Name: s, State: "unknown"}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := exec.CommandContext(cctx, "systemctl", "is-active", s).Output()
		cancel()
		if err == nil {
			st.State = strings.TrimSpace(string(b))
			if st.State == "" {
				st.State = "unknown"
			}
		}
		out = append(out, st)
	}
	return out
}
