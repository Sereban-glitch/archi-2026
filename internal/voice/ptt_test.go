// ptt_test.go — тесты релиза 2026.16 «Push-To-Talk»: VAD-автостоп по хвосту
// тишины, синтез бипов (заголовок WAV, рампы), lock-тоггл повторного нажатия.
package voice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/archi/archi/internal/config"
)

// ---- синтетика PCM ----

// quietFrame — кадр «тихой комнаты» (RMS ≈ amp/√2, ниже стартового порога).
func quietFrame(rate, ms int) []byte { return toneFrame(30, rate, ms) }

// speechFrame — кадр громкой речи (RMS ≈ 2100 ≫ порога).
func speechFrame(rate, ms int) []byte { return toneFrame(3000, rate, ms) }

// toneFrame — синус амплитуды amp длительностью ms.
func toneFrame(amp float64, rate, ms int) []byte {
	n := rate / 1000 * ms
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp * math.Sin(2*math.Pi*220*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(v)))
	}
	return pcm
}

func TestPTTVADTrailingSilenceStops(t *testing.T) {
	v := newPTTVAD(config.PTTConfig{SilenceMs: 800, StartFactor: 2.0})
	const rate, ms = 16000, 30

	// 1) тишина до речи: пол адаптируется, стопа нет.
	for i := 0; i < 10; i++ {
		if v.feed(rms(quietFrame(rate, ms))) {
			t.Fatalf("стоп в тишине до речи (кадр %d)", i)
		}
	}
	if v.SpeechStarted() {
		t.Fatal("речь не должна была начаться")
	}

	// 2) речь 360 мс ≥ minSpeech (240 мс) — речь признана.
	for i := 0; i < 12; i++ {
		if v.feed(rms(speechFrame(rate, ms))) {
			t.Fatalf("стоп во время речи (кадр %d)", i)
		}
	}
	if !v.SpeechStarted() {
		t.Fatal("речь должна была быть признана после 360 мс")
	}

	// 3) хвост тишины: стоп на 27-м кадре (810 мс ≥ 800 мс), не раньше 26-го.
	stoppedAt := -1
	for i := 0; i < 40; i++ {
		if v.feed(rms(quietFrame(rate, ms))) {
			stoppedAt = i + 1 // номер кадра тишины, 1-based
			break
		}
	}
	if stoppedAt < 0 {
		t.Fatal("автостоп по хвосту тишины не сработал за 1200 мс")
	}
	if stoppedAt < 26 || stoppedAt > 28 {
		t.Fatalf("автостоп на кадре %d (ожидался 26..28 = 780..840 мс)", stoppedAt)
	}
}

func TestPTTVADPureSilenceNeverStops(t *testing.T) {
	v := newPTTVAD(config.PTTConfig{SilenceMs: 800, StartFactor: 2.0})
	const rate, ms = 16000, 30
	for i := 0; i < 100; i++ { // 3 секунды тишины
		if v.feed(rms(quietFrame(rate, ms))) {
			t.Fatal("тишина без речи не должна останавливать запись")
		}
	}
	if v.SpeechStarted() {
		t.Fatal("не было речи — SpeechStarted обязан быть false")
	}
	// пол стянулся к тишине, но не ниже clamp 20.
	if v.floor < 20 || v.floor > 40 {
		t.Fatalf("пол после тишины = %.1f, ожидался в коридоре 20..40", v.floor)
	}
}

func TestPTTVADShortBlipNotSpeech(t *testing.T) {
	v := newPTTVAD(config.PTTConfig{SilenceMs: 800, StartFactor: 2.0})
	const rate, ms = 16000, 30
	// клик/стук 90 мс < minSpeech — речью не считается.
	for i := 0; i < 3; i++ {
		v.feed(rms(speechFrame(rate, ms)))
	}
	for i := 0; i < 40; i++ {
		if v.feed(rms(quietFrame(rate, ms))) {
			t.Fatal("клик короче minSpeech не должен армировать автостоп")
		}
	}
	if v.SpeechStarted() {
		t.Fatal("90 мс шума — это не речь")
	}
}

func TestSynthBeepWAV(t *testing.T) {
	wav := synthBeep(0.2, 90, 880, 1174.66)
	if len(wav) != 44+1440*2*2 {
		t.Fatalf("размер WAV %d, ожидался %d (2 тона по 90 мс)", len(wav), 44+1440*2*2)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[36:40]) != "data" {
		t.Fatal("битый WAV-заголовок")
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != beepRate {
		t.Fatalf("частота %d, ожидалась %d", got, beepRate)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 1440*2*2 {
		t.Fatalf("data-чанк %d байт, ожидался %d", got, 1440*2*2)
	}
	// амплитуда: пик ограничен рампой, сигнал ненулевой (сэмплы — после заголовка).
	peak := 0
	for i := 44; i+1 < len(wav); i += 2 {
		s := int(int16(binary.LittleEndian.Uint16(wav[i:])))
		if s > peak {
			peak = s
		}
	}
	soft := int(math.Round(0.22 * 32767)) // амплитуда бипа 0.2 + запас
	if peak < 1000 || peak > soft {
		t.Fatalf("пик бипа %d — вне коридора мягкости (верх %d)", peak, soft)
	}
	for _, b := range [][]byte{beepStart, beepStop, beepError} {
		if len(b) < 44 {
			t.Fatal("глобальный бип пуст")
		}
	}
}

func TestPTTLockAcquireReleaseStale(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	l, err := acquirePTTLock()
	if err != nil || l == nil {
		t.Fatalf("первый захват: %v", err)
	}
	path := l.path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock-файл не создан: %v", err)
	}
	l.release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("release не убрал lock-файл")
	}

	// протухший lock (мёртвый pid) — подбирается молча.
	if err := os.WriteFile(path, []byte("4194301"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := acquirePTTLock()
	if err != nil || l2 == nil {
		t.Fatalf("stale lock не подобрался: %v", err)
	}
	l2.release()

	// мусор в lock-файле — тоже подбирается.
	if err := os.WriteFile(path, []byte("не pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	l3, err := acquirePTTLock()
	if err != nil || l3 == nil {
		t.Fatalf("битый lock не подобрался: %v", err)
	}
	l3.release()
}

// TestPTTLockSignalsLivePeer — повторное нажатие видит живую сессию и шлёт ей
// SIGUSR1. Живая сессия — дочерний процесс этого же тест-бинаря (cmdline
// содержит имя теста с «ptt», поэтому pttProcAlive признаёт его своим).
func TestPTTLockSignalsLivePeer(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		// ребёнок: живая ptt-сессия, ловит SIGUSR1 и уходит.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGUSR1)
		fmt.Println("READY")
		<-ch
		fmt.Println("SIGNALED")
		os.Exit(0)
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	cmd := exec.Command(os.Args[0], "-test.run=TestPTTLockSignalsLivePeer")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("старт ребёнка: %v", err)
	}
	defer cmd.Wait()

	// ждём готовности ребёнка (читаем до токена, с таймаутом).
	if !waitToken(out, "READY", 5*time.Second) {
		cmd.Process.Kill()
		t.Fatal("ребёнок не отчитался о готовности")
	}

	// lock «принадлежит» ребёнку — второй запуск должен его остановить.
	path := pttLockPath()
	if err := os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = acquirePTTLock()
	if !errors.Is(err, ErrPTTSignaled) {
		cmd.Process.Kill()
		t.Fatalf("ожидался ErrPTTSignaled, получено: %v", err)
	}

	// ребёнок должен получить сигнал и выйти с «SIGNALED».
	if !waitToken(out, "SIGNALED", 5*time.Second) {
		t.Fatal("ребёнок не получил SIGUSR1")
	}
}

// waitToken читает r, пока не встретит token или не истечёт timeout.
func waitToken(r io.Reader, token string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	acc := ""
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := r.Read(buf)
		if n > 0 {
			acc += string(buf[:n])
			if strings.Contains(acc, token) {
				return true
			}
		}
		if err != nil {
			return strings.Contains(acc, token)
		}
	}
	return strings.Contains(acc, token)
}

// TestPTTLockPathIsolation — lock живёт в XDG_RUNTIME_DIR и именован по uid.
func TestPTTLockPathIsolation(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/xdgtest")
	p := pttLockPath()
	want := filepath.Join("/tmp/xdgtest", fmt.Sprintf("archi-ptt-%d.lock", os.Getuid()))
	if p != want {
		t.Fatalf("путь %q, ожидался %q", p, want)
	}
}
