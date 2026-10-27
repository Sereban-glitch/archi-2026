package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// RunCLI — клиент для терминала: `archi ctl "текст запроса"`.
// Компилируется в тот же main с подкомандой ctl (см. cmd/archi/main.go).
// Возвращает ответ агента (также печатает его в stdout) — нужно для `archi voice --speak`.
func RunCLI(socketPath, text string, reset, status bool) (string, error) {
	reply, err := RunCLIQuiet(socketPath, text, reset, status)
	if err != nil {
		return "", err
	}
	fmt.Println(reply)
	return reply, nil
}

// RunCLIQuiet — то же без печати в stdout: для фоновых потребителей
// (ear-демона, `archi voice`), где ответ уходит дальше по конвейеру.
func RunCLIQuiet(socketPath, text string, reset, status bool) (string, error) {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("не подключиться к %s (archi запущен?): %w", socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))

	req := request{Text: text, Reset: reset, Status: status}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("отправка: %w", err)
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("чтение ответа: %w", err)
		}
		return "", fmt.Errorf("сервер закрыл соединение без ответа")
	}
	var resp response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("битый ответ: %w (фрагмент: %s)", err, clip(sc.Text(), 200))
	}
	if !resp.OK {
		return "", fmt.Errorf("агент: %s", resp.Error)
	}
	return resp.Reply, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
