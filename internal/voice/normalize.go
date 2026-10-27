// normalize.go — текстовая нормализация перед синтезом речи (2026.15 «Живой голос»).
//
// Агент отвечает админ-markdown'ом: списки, «- CPU:», «2.56», «GiB», «76%», эмодзи,
// URL. Синтезатор же читает это буквально: «два ТОЧКА пятьдесят шесть», «тире»,
// «джиби-байт» по буквам. NormalizeForSpeech превращает отчёт в живую речь:
//   - «2.56»         → «2 и 56»            (десятичная точка/запятая не читается)
//   - «76%»          → «76 процентов»
//   - «GiB»/«GB»     → «гигабайт» (MiB/KB/TiB… — весь словарь единиц)
//   - «CPU»          → «процессор», «RAM» → «память», «GPU» → «видеокарта»
//   - маркеры списка «- », «* », «1. » → мягкая запятая
//   - markdown (жирный/курсив/код/ссылки/таблицы) и эмодзи убираются
//   - URL            → «ссылка»
//
// SpeechChunks режет результат на строки-предложения — по одной на строку stdin
// Piper: движок озвучивает первую фразу, пока синтезирует остальные (стриминг).
package voice

import (
	"regexp"
	"strings"
	"unicode"
)

// ---- скомпилированные правила (пакет-левел: раз компиляция — навсегда) ----

var (
	// код-фенсы ``` и ```lang — маркеры строк убираем
	reCodeFence = regexp.MustCompile("(?m)^```.*$")

	// цитаты «> текст» в начале строки
	reQuote = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)

	// маркеры списков в начале строки: «- », «* », «• », «1. », «2) »
	reListItem = regexp.MustCompile(`(?m)^[ \t]*(?:[-*•·‣▪]|[0-9]{1,3}[.)])[ \t]+`)

	// заголовки «### Текст» и горизонтальные линии «---», «===», «***»
	reHeading = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6})[ \t]+`)
	// три и более -, =, *, _ (с пробелами) на строке — во весь ряд
	reHR = regexp.MustCompile(`(?m)^[ \t]*(?:[-=*_][ \t]*){3,}$`)

	// markdown-ссылка [текст](урл) → текст; картинки ![alt](урл) → alt
	reLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)

	// выделения: **жирный**, __жирный__, *курсив*, ~~зачёркнутый~~ (RE2 без backref)
	reEmph = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__|\*(.+?)\*|~~(.+?)~~`)
	reCode = regexp.MustCompile("`([^`]*)`")

	// URL любой формы — зачитывать адрес бессмысленно
	reURL = regexp.MustCompile(`(?:https?://|www\.)[^\s<>"{}|\\^` + "`" + `\[\]]+`)

	// десятичные: «2.56» / «2,56» → «2 и 56» (целая и дробная части — цифры)
	reDecimal = regexp.MustCompile(`(\d+)[.,](\d+)`)

	// диапазон: «10-20» → «10 — 20» (тире читается паузой, а не словом)
	reRange = regexp.MustCompile(`(\d)[ \t]*-[ \t]*(\d)`)

	// стрелки и следствия → тире-пауза
	reArrow = regexp.MustCompile(`(?:->|=>|→|⇒)`)

	// время/пары «15:04» → «15 04» (прочтётся «пятнадцать ноль четыре»)
	reTime = regexp.MustCompile(`(\d):(\d)`)

	// эмодзи и прочие «другие символы» юникода (✅🔴🟡🤖…) + вариационные селекторы
	reEmoji = regexp.MustCompile("[\U0001F000-\U0001FAFF\u2600-\u27BF\uFE0E\uFE0F\u200D]")

	// финальная чистка пробелов
	reSpace  = regexp.MustCompile(`[ \t]+`)
	reNewine = regexp.MustCompile(`\n{3,}`)
)

// unitWords — технические единицы и аббревиатуры словами.
// Регистр важен (GiB ≠ gib): паттерны границей слова, case-sensitive.
var unitWords = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`\bGiB(?:/s)?\b`), " гигабайт"}, //  упрощённо: скорость -> «гигабайт»
	{regexp.MustCompile(`\bMiB(?:/s)?\b`), " мегабайт"},
	{regexp.MustCompile(`\bKiB(?:/s)?\b`), " килобайт"},
	{regexp.MustCompile(`\bTiB(?:/s)?\b`), " терабайт"},
	{regexp.MustCompile(`\bGB/s\b`), " гигабайт в секунду"},
	{regexp.MustCompile(`\bMB/s\b`), " мегабайт в секунду"},
	{regexp.MustCompile(`\bKB/s\b`), " килобайт в секунду"},
	{regexp.MustCompile(`\bTB\b`), " терабайт"},
	{regexp.MustCompile(`\bGB\b`), " гигабайт"},
	{regexp.MustCompile(`\bMB\b`), " мегабайт"},
	{regexp.MustCompile(`\bKB\b`), " килобайт"},
	{regexp.MustCompile(`\bGHz\b`), " гигагерц"},
	{regexp.MustCompile(`\bMHz\b`), " мегагерц"},
	{regexp.MustCompile(`\bCPU\b`), "процессор"},
	{regexp.MustCompile(`\bRAM\b`), "память"},
	{regexp.MustCompile(`\bGPU\b`), "видеокарта"},
	{regexp.MustCompile(`\bSSD\b`), "твердотельный диск"},
	{regexp.MustCompile(`\bHDD\b`), "жёсткий диск"},
	{regexp.MustCompile(`\bMiB/s\b`), " мегабайт в секунду"},
	{regexp.MustCompile(`°C\b`), " градусов"},
}

// NormalizeForSpeech — главный вход: админ-текст → то, что приятно слышать.
// Идемпотентна: повторный прогон ничего не меняет.
func NormalizeForSpeech(s string) string {
	// 1. Структурный markdown: фенсы, заголовки, hr, списки, таблицы.
	s = reCodeFence.ReplaceAllString(s, "")
	s = reHeading.ReplaceAllString(s, "")
	s = reHR.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = reListItem.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1")

	// 2. Таблицы: «|» между ячейками → запятая (строки с двумя и более палками).
	if strings.Count(s, "|") >= 2 {
		s = strings.ReplaceAll(s, "|", ",")
	}

	// 3. Выделения и код — только содержимое.
	s = reEmph.ReplaceAllString(s, "$1$2$3$4")
	s = reCode.ReplaceAllString(s, "$1")

	// 4. URL → «ссылка» (после markdown-ссылок, чтобы не задеть их текст).
	s = reURL.ReplaceAllString(s, "ссылка")

	// 5. Единицы и аббревиатуры.
	for _, u := range unitWords {
		s = u.re.ReplaceAllString(s, u.to)
	}

	// 6. Проценты: «76%» → «76 процентов».
	s = regexp.MustCompile(`(\d)\s*%`).ReplaceAllString(s, "$1 процентов")

	// 7. Числа: десятичные, диапазоны, время, стрелки.
	s = reDecimal.ReplaceAllString(s, "$1 и $2")
	s = reRange.ReplaceAllString(s, "$1 — $2")
	s = reArrow.ReplaceAllString(s, " — ")
	s = reTime.ReplaceAllString(s, "$1 $2")

	// 8. Хвостовые символы, которые читаются мусором.
	s = strings.NewReplacer("~", " примерно ", "_", " ", "=", " ", "#", "",
		"…", "...", "\u00a0", " ").Replace(s)

	// 9. Эмодзи и графические символы.
	s = reEmoji.ReplaceAllString(s, "")

	// 10. Пробелы в норму; пустые строки сжимаем (кандидаты в фразы).
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, " ,", ",")
	s = reNewine.ReplaceAllString(s, "\n\n")
	s = strings.Trim(s, " \t\n")
	return s
}

// SpeechChunks — режет нормализованный текст на строки-фразы для потоковой
// озвучки: Piper читает stdin построчно и стримит аудио по мере синтеза,
// поэтому первая фраза звучит задолго до готовности всего текста.
// Предложения длиннее maxRunes делятся по запятым/тире/пробелам — иначе
// длинная фраза задерживает старт звука.
func SpeechChunks(s string, maxRunes int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 200
	}

	// 1. Границы предложений: [.!?…] (+ закрывающие кавычки/скобки).
	sentEnd := func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '…'
	}
	var sentences []string
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			sentences = append(sentences, t)
		}
		cur.Reset()
	}
	runes := []rune(s)
	for i, r := range runes {
		cur.WriteRune(r)
		if sentEnd(r) {
			// не разрываем «1.5» и многоточия: смотрим, что дальше
			if i+1 < len(runes) && (unicode.IsDigit(runes[i+1]) || runes[i+1] == '.') {
				continue
			}
			flush()
		}
	}
	flush()

	// 2. Длинные предложения режем по запятым, тире, потом по словам.
	var out []string
	for _, sent := range sentences {
		if len([]rune(sent)) <= maxRunes {
			out = append(out, sent)
			continue
		}
		for _, part := range splitLong(sent, maxRunes) {
			if strings.TrimSpace(part) != "" {
				out = append(out, strings.TrimSpace(part))
			}
		}
	}
	if len(out) == 0 {
		out = []string{s}
	}
	return out
}

// splitLong делит слишком длинную фразу по границам дыхания: «, » → « — » → пробел.
func splitLong(sent string, maxRunes int) []string {
	sepAt := func(sep string) (parts []string, ok bool) {
		idx := []int{}
		for i := 0; i < len(sent); i++ {
			if strings.HasPrefix(sent[i:], sep) {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return nil, false
		}
		// делим на куски ≤ maxRunes: жадно набираем до последней подходящей границы
		start := 0
		for start < len(sent) {
			limit := start + maxRunes
			if limit >= len(sent) {
				parts = append(parts, sent[start:])
				break
			}
			best := -1
			for _, i := range idx {
				if i > start && i+1 <= limit {
					best = i
				}
			}
			if best <= start { // граница не нашлась — режем по пробелу
				best = strings.LastIndex(sent[start:limit], " ") + start
				if best <= start {
					best = limit
				}
				parts = append(parts, sent[start:best])
				start = best
				continue
			}
			cut := best + len(sep)
			parts = append(parts, sent[start:cut])
			start = cut
		}
		return parts, true
	}
	for _, sep := range []string{", ", " — ", "; "} {
		if parts, ok := sepAt(sep); ok && len(parts) > 1 {
			return parts
		}
	}
	// совсем без границ — по пробелам
	var parts []string
	r := []rune(sent)
	for len(r) > maxRunes {
		cut := maxRunes
		for cut > 0 && r[cut-1] != ' ' {
			cut--
		}
		if cut == 0 {
			cut = maxRunes
		}
		parts = append(parts, strings.TrimSpace(string(r[:cut])))
		r = r[cut:]
	}
	parts = append(parts, strings.TrimSpace(string(r)))
	return parts
}
