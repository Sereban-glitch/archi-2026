package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/archi/archi/internal/bridge"
)

// forbiddenRead — не читаем секретные пути (паттерны прекомпилированы в NewRegistry;
// раньше компилировались на каждый вызов — аудит 2026-10).
func (r *Registry) forbiddenRead(path string) bool {
	for _, re := range r.forbidden {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// writeAllowed проверяет, что путь попадает в разрешённые для записи каталоги.
// Симлинки раскрываются: ~/link → /etc/passhdr не должен обмануть префиксную проверку.
func (r *Registry) writeAllowed(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		// файла может ещё не быть — раскрываем ближайшего существующего родителя
		abs = filepath.Join(resolvedDir, filepath.Base(abs))
	}
	for _, dir := range r.deps.Cfg.Tools.AllowedWriteDirs {
		d, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(abs+string(filepath.Separator), d+string(filepath.Separator)) || abs == d {
			return true
		}
	}
	return false
}

// registerFS — read_file / write_file / list_directory с указанием ноды.
func (r *Registry) registerFS() {
	r.Register(&Tool{
		Name: "read_file",
		Description: "Прочитать текстовый файл. Работает на обеих нодах: local (ноутбук) и remote (VM). " +
			"Секреты (.ssh, приватные ключи, shadow) читать запрещено.",
		Timeout: 30 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node":   nodeEnum(),
				"path":   map[string]any{"type": "string", "description": "абсолютный путь к файлу"},
				"offset": map[string]any{"type": "integer", "description": "начать со строки (1-based, опционально)"},
				"limit":  map[string]any{"type": "integer", "description": "максимум строк"},
			},
			"required": []string{"node", "path"},
		},
		Exec: r.execReadFile,
	})
	r.Register(&Tool{
		Name: "write_file",
		Description: "Записать текстовый файл (перезапись или append). Только в разрешённых каталогах: " +
			strings.Join([]string{"/home", "/tmp", "/var/lib/archi", "/srv"}, ", ") + ".",
		Timeout: 30 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node":    nodeEnum(),
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
				"append":  map[string]any{"type": "boolean", "description": "дописать в конец вместо перезаписи"},
			},
			"required": []string{"node", "path", "content"},
		},
		Exec: r.execWriteFile,
	})
	r.Register(&Tool{
		Name:        "list_directory",
		Description: "Листинг каталога на любой ноде (имя, размер, режим, mtime).",
		Timeout:     20 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node": nodeEnum(),
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"node", "path"},
		},
		Exec: r.execListDir,
	})
}

func (r *Registry) execReadFile(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	path := argString(args, "path", "")
	if path == "" {
		return "ОШИБКА: path обязателен", true
	}
	if r.forbiddenRead(path) {
		return "ОШИБКА: путь содержит секреты (приватные ключи/учётные данные) — чтение запрещено политикой", true
	}
	offset := argInt(args, "offset", 1)
	limit := argInt(args, "limit", 400)

	if node == bridge.NodeLocal {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("ОШИБКА: %v", err), true
		}
		return clipLines(string(data), offset, limit, r.deps.Cfg.Tools.MaxReadBytes), false
	}
	// remote: sed по строкам внутри одной SSH-команды
	lo, hi := offset, offset+limit-1
	cmd := fmt.Sprintf("sed -n '%d,%dp' -- %s", lo, hi, shellQ(path))
	res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 30*time.Second, r.deps.Cfg.Bridge.MaxOutputBytes)
	if err != nil {
		return fmt.Sprintf("ОШИБКА SSH: %v", err), true
	}
	if res.ExitCode != 0 && strings.TrimSpace(res.Stdout) == "" {
		return formatExecResult(res), true
	}
	return "path=" + path + " строки " + fmt.Sprint(lo) + "-" + fmt.Sprint(hi) + "\n" + res.Stdout, res.ExitCode != 0
}

func (r *Registry) execWriteFile(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	path := argString(args, "path", "")
	content := argString(args, "content", "")
	appendMode := argBool(args, "append", false)
	if path == "" {
		return "ОШИБКА: path обязателен", true
	}
	if !r.writeAllowed(path) {
		return "ОШИБКА: путь вне разрешённых для записи каталогов (" +
			strings.Join(r.deps.Cfg.Tools.AllowedWriteDirs, ", ") + ")", true
	}

	if node == bridge.NodeLocal {
		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if appendMode {
			flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		f, err := os.OpenFile(path, flag, 0o644)
		if err != nil {
			return fmt.Sprintf("ОШИБКА: %v", err), true
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return fmt.Sprintf("ОШИБКА записи: %v", err), true
		}
		return fmt.Sprintf("OK: записано %d байт в %s (local, append=%v)", len(content), path, appendMode), false
	}

	// remote: безопасная передача содержимого через base64 в одной SSH-команде
	op := ">"
	if appendMode {
		op = ">>"
	}
	b64 := b64encode(content)
	cmd := fmt.Sprintf("echo %s | base64 -d %s %s && echo WRITTEN $(echo %s | base64 -d | wc -c) BYTES", shellQ(b64), op, shellQ(path), shellQ(b64))
	res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 60*time.Second, 16*1024)
	if err != nil {
		return fmt.Sprintf("ОШИБКА SSH: %v", err), true
	}
	if res.ExitCode != 0 {
		return formatExecResult(res), true
	}
	return "OK: " + strings.TrimSpace(res.Stdout) + " (remote)", false
}

func (r *Registry) execListDir(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	path := argString(args, "path", "")
	if path == "" {
		return "ОШИБКА: path обязателен", true
	}
	// time-style собираем конкатенацией, чтобы go vet не считал %Y формат-глаголом
	tsStyle := "+%Y" + "-%m-%dT%H:%M"
	cmd := fmt.Sprintf("ls -lA --time-style=%s -- %s 2>&1 | head -c %d", tsStyle, shellQ(path), r.deps.Cfg.Bridge.MaxOutputBytes)
	res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 30*time.Second, r.deps.Cfg.Bridge.MaxOutputBytes)
	if err != nil {
		return fmt.Sprintf("ОШИБКА SSH: %v", err), true
	}
	out := "path=" + path + " node=" + node + "\n" + res.Stdout
	if s := strings.TrimSpace(res.Stderr); s != "" {
		out += "\nstderr: " + s
	}
	return out, res.ExitCode != 0
}

// clipLines выбирает строки [offset, offset+limit) и урезает по байтам.
func clipLines(s string, offset, limit, maxBytes int) string {
	if offset < 1 {
		offset = 1
	}
	lines := strings.Split(s, "\n")
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	if offset-1 > len(lines) {
		return "(пусто: offset за пределами файла)"
	}
	out := strings.Join(lines[offset-1:end], "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes] + "\n…[урезано]"
	}
	return fmt.Sprintf("строки %d-%d из %d:\n%s", offset, end, len(lines), out)
}

func argBool(args json.RawMessage, key string, def bool) bool {
	if len(args) == 0 {
		return def
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return def
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

func shellQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\\''`) + "'" }

const b64alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// b64encode — стандартный base64 без внешних зависимостей на стороне сборки схемы.
func b64encode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i += 3 {
		var b [3]byte
		n := copy(b[:], s[i:])
		sb.WriteByte(b64alphabet[b[0]>>2])
		sb.WriteByte(b64alphabet[(b[0]&0x03)<<4|b[1]>>4])
		if n > 1 {
			sb.WriteByte(b64alphabet[(b[1]&0x0f)<<2|b[2]>>6])
		} else {
			sb.WriteByte('=')
		}
		if n > 2 {
			sb.WriteByte(b64alphabet[b[2]&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}
