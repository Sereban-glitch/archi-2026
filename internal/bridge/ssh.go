package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/archi/archi/internal/config"
)

// SSHPool — пул отказоустойчивых SSH-соединений к agrobot-prod.
// Особенности (выжимка из практик 2026):
//   - keep-alive проверка перед выдачей соединения из пула;
//   - фоновый пинг каждые KeepaliveInterval, мёртвые соединения выбрасываются;
//   - повторный набор (dial) с экспоненциальным backoff + джиттер;
//   - изоляция: каждая команда — отдельная SSH-сессия.
type SSHPool struct {
	cfg config.SSHConfig
	log *slog.Logger

	mu    sync.Mutex
	idle  []*ssh.Client
	sem   chan struct{}
	fails int // подряд идущие неудачные dial-ы (для backoff)
}

func newSSHPool(cfg config.SSHConfig, log *slog.Logger) *SSHPool {
	if cfg.PoolSize < 1 {
		cfg.PoolSize = 1
	}
	return &SSHPool{cfg: cfg, log: log, sem: make(chan struct{}, cfg.PoolSize)}
}

func (p *SSHPool) addr() string {
	return net.JoinHostPort(p.cfg.Host, fmt.Sprintf("%d", p.cfg.Port))
}

// authMethods собирает методы аутентификации: файл ключа → ssh-agent (fallback).
func (p *SSHPool) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if p.cfg.KeyPath != "" {
		data, err := os.ReadFile(p.cfg.KeyPath)
		if err != nil {
			p.log.Warn("ssh: не читается ключ, пробую ssh-agent", "path", p.cfg.KeyPath, "err", err)
		} else {
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				// ключ с passphrase — пробуем агент
				p.log.Warn("ssh: ключ не парсится (passphrase?), пробую ssh-agent", "err", err)
			} else {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(c).Signers))
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("нет доступных SSH-методов аутентификации: ключ %q недоступен, ssh-agent не найден", p.cfg.KeyPath)
	}
	return methods, nil
}

// hostKeyCallback политику проверки хоста.
func (p *SSHPool) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if !p.cfg.StrictHostKey {
		p.log.Warn("ssh: strict_host_key=false — проверка отпечатка ОТКЛЮЧЕНА (только для доверенных сетей)")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	cb, err := knownhosts.New(p.cfg.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("known_hosts %s: %w", p.cfg.KnownHosts, err)
	}
	return cb, nil
}

// dial устанавливает новое соединение с учётом контекста и таймаута хендшейка.
func (p *SSHPool) dial(ctx context.Context) (*ssh.Client, error) {
	auth, err := p.authMethods()
	if err != nil {
		return nil, err
	}
	hkcb, err := p.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	sshCfg := &ssh.ClientConfig{
		User:            p.cfg.User,
		Auth:            auth,
		HostKeyCallback: hkcb,
		Timeout:         p.cfg.ConnectTimeout.Duration,
	}
	type res struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan res, 1)
	go func() {
		d := net.Dialer{Timeout: p.cfg.ConnectTimeout.Duration}
		conn, err := d.DialContext(ctx, "tcp", p.addr())
		if err != nil {
			ch <- res{nil, fmt.Errorf("tcp: %w", err)}
			return
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, p.addr(), sshCfg)
		if err != nil {
			conn.Close()
			ch <- res{nil, fmt.Errorf("handshake: %w", err)}
			return
		}
		ch <- res{ssh.NewClient(c, chans, reqs), nil}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		// фоновый dial завершится сам и попадёт в ch (буфер 1 — утечки нет)
		return nil, ctx.Err()
	}
}

// healthy делает keep-alive запрос и закрывает мёртвое соединение.
func (p *SSHPool) healthy(ctx context.Context, c *ssh.Client) bool {
	if c == nil {
		return false
	}
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		ch <- res{err}
	}()
	select {
	case r := <-ch:
		return r.err == nil
	case <-time.After(5 * time.Second):
		return false
	case <-ctx.Done():
		return false
	}
}

// get возвращает живое соединение из пула или набирает новое (с backoff после серий неудач).
func (p *SSHPool) get(ctx context.Context) (*ssh.Client, error) {
	// 1) пробуем idle-соединения
	for {
		p.mu.Lock()
		var c *ssh.Client
		if len(p.idle) > 0 {
			c = p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
		}
		p.mu.Unlock()
		if c == nil {
			break
		}
		if p.healthy(ctx, c) {
			return c, nil
		}
		p.log.Info("ssh: idle-соединение мертво, закрываю")
		c.Close()
	}
	// 2) набираем новое с ограничением параллелизма и backoff
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.sem }()

	p.mu.Lock()
	fails := p.fails // атомарное чтение под локом (аудит 2026-10: data race)
	p.mu.Unlock()
	if fails > 0 {
		// экспоненциальный backoff с джиттером: 1s, 2s, 4s… максимум 30s
		d := time.Duration(1) << uint(min(fails-1, 5)) * time.Second
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		d += time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
		p.log.Warn("ssh: жду перед повторным набором соединения", "backoff", d.String(), "fails", fails)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c, err := p.dial(ctx)
	p.mu.Lock()
	if err != nil {
		p.fails++
	} else {
		p.fails = 0
	}
	p.mu.Unlock()
	return c, err
}

// put возвращает соединение в пул (лишние закрываются).
func (p *SSHPool) put(c *ssh.Client) {
	if c == nil {
		return
	}
	p.mu.Lock()
	if len(p.idle) < p.cfg.PoolSize {
		p.idle = append(p.idle, c)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	c.Close()
}

// keepaliveLoop — фоновый прогрев idle-соединений (держит NAT/файрвол открытми).
func (p *SSHPool) keepaliveLoop(ctx context.Context) {
	if p.cfg.KeepaliveInterval.Duration <= 0 {
		return
	}
	t := time.NewTicker(p.cfg.KeepaliveInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			idle := append([]*ssh.Client(nil), p.idle...)
			p.idle = nil
			p.mu.Unlock()
			for _, c := range idle {
				if p.healthy(ctx, c) {
					p.put(c)
				} else {
					c.Close()
				}
			}
		}
	}
}

// Run выполняет команду на удалённой ноде через выделенную SSH-сессию.
// Ретраи — только на транспортных ошибках (dial/session-level), не на exit-коде команды.
func (p *SSHPool) Run(ctx context.Context, command string, maxOutput int) (*ExecResult, error) {
	var lastErr error
	attempts := p.cfg.MaxRetries
	if attempts < 1 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-time.After(time.Duration(i) * 500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c, err := p.get(ctx)
		if err != nil {
			lastErr = fmt.Errorf("ssh get: %w", err)
			continue
		}
		res, retryable, err := p.runOnce(ctx, c, command, maxOutput)
		if err == nil {
			p.put(c)
			return res, nil
		}
		c.Close() // после ошибки сессии соединение не переиспользуем
		if !retryable {
			return res, err
		}
		lastErr = err
		p.log.Warn("ssh: транспортная ошибка, повтор", "attempt", i+1, "err", err)
	}
	return nil, fmt.Errorf("ssh run: все попытки исчерпаны: %w", lastErr)
}

// runOnce — одна попытка исполнения. Возвращает (результат, retryable, error).
func (p *SSHPool) runOnce(ctx context.Context, c *ssh.Client, command string, maxOutput int) (*ExecResult, bool, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, true, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	stdout, stderr := newLimitedBuf(maxOutput), newLimitedBuf(maxOutput)
	sess.Stdout, sess.Stderr = stdout, stderr

	start := time.Now()
	if err := sess.Start(command); err != nil {
		return nil, true, fmt.Errorf("start: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- sess.Wait() }()

	select {
	case werr := <-waitCh:
		res := &ExecResult{Duration: time.Since(start)}
		res.Stdout, res.Truncated = stdout.String()
		res.Stderr, _ = stderr.String()

		// Нормальный путь: Wait вернул *ssh.ExitError с кодом.
		var exitErr *ssh.ExitError
		switch {
		case werr == nil:
			res.ExitCode = 0
			return res, false, nil
		case errors.As(werr, &exitErr):
			res.ExitCode = exitErr.ExitStatus()
			return res, false, nil
		case errors.Is(werr, io.EOF) && ctx.Err() == nil:
			// Известная особенность x/crypto/ssh против OpenSSH: Wait может вернуть
			// io.EOF без статуса выхода (golang/go#38115). Вывод был — считаем успешным.
			res.ExitCode = 0
			res.Stderr += "\n[archi] примечание: сервер не передал exit-статус (EOF)"
			return res, false, nil
		}
		if ctx.Err() != nil {
			return res, false, ctx.Err()
		}
		return res, true, fmt.Errorf("wait: %w", werr)
	case <-ctx.Done():
		// убиваем удалённый процесс и сессию, не дожидаясь Wait
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		res := &ExecResult{Duration: time.Since(start)}
		res.Stdout, _ = stdout.String()
		res.Stderr, _ = stderr.String()
		res.ExitCode = -9
		res.Stderr += "\n[archi] команда прервана (таймаут/отмена)"
		return res, false, ctx.Err()
	}
}

// Close закрывает все idle-соединения пула.
func (p *SSHPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.idle {
		c.Close()
	}
	p.idle = nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
