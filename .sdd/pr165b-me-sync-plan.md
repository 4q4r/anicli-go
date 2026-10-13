<!-- markdownlint-disable MD010 -->
<!-- MD010 (hard tabs) отключён локально: Go-скичи в fenced-блоках обязаны
     использовать табы (gofmt); пробелы сломали бы вставку кода исполнителем. -->

# PR165b — план реализации «кросс-корреляция M&E» (автосинхронизация дубляжей)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** автоматическая правка рассинхрона дубляжа относительно видео через FFT-кросс-корреляцию аудио (decode → correlate → offset → mux), с copy-путём по умолчанию и фильтр-путём владельца v2 (PTS-нормализация) для HLS/фолбэка.

**Architecture:** новый чистый пакет `internal/audsync` (ffmpeg-pipe PCM reader 8 кГц моно s16le → gonum FFT кросс-корреляция в 3 окнах → консенсус ≥2/3 ±80 мс → медиана → snap на сетку кадров → типизированный `SyncOffset`). Применение — в `internal/download` (argv-билдеры: `-ss`/`itsoffset`+copy для прямых источников, фильтр-цепочка владельца v2 + `aac 256k` для HLS). Интеграция — один хук в `tui/real.go downloadOne` (после resolve, до mux), референс декодируется один раз на эпизод.

**Tech Stack:** Go 1.27, `gonum.org/v1/gonum/dsp/fourier` (v0.17.0, чистый Go, CGO off), ffmpeg/ffprobe n9+ (уже системная зависимость проекта), существующие пакеты download/tui/config/i18n.

**Spec:** `.sdd/pr165b-me-sync-research.md` (исследование: алгоритм, измерения, рецепты; ЗДЕСЬ не воспроизводится — все константы и argv скопированы в этот план). ADDENDUM владельца от 2026-10-13: «паразитная тишина» на HLS-донорах → **фильтр-путь = рецепт владельца v2** (normalize → apply → re-normalize); copy-путь для HLS подлежит верификационному пробу (Task 6).

## Global Constraints

- Окружение для КАЖДОЙ go-команды: `export TMPDIR=/home/whoami/.cache/go-tmp GOTMPDIR=/home/whoami/.cache/go-tmp && mkdir -p $TMPDIR`.
- Quality gate на каждый таск: `gofmt -l` (пусто) → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` — 0 ошибок, 0 предупреждений.
- Кросс-платформа: `make build-matrix` (CGO_ENABLED=0) должен проходить — gonum чистый Go, это безопасно.
- Анализ **никогда не валит загрузку**: любая ошибка анализа → mux как сегодня (без сдвига) + строка в diagnostics-лог.
- Бюджет анализа: ≤15 c на пару (видео, дубляж), константа `DefaultBudget`.
- Корреляция только для пар «видео-провайдер ≠ провайдер дубляжа»; встроенное аудио видео — не анализировать.
- Sign-семантика (владелец): `offset > 0` — у донора «лишнее начало» → обрезать; `offset < 0` — донор отстаёт → долить тишину. Форматирование секунд — 3 знака после запятой (`17.042`, `0.958`).
- Фильтр-путь — **всегда** рецепт владельца v2 (ADDENDUM): `asetpts=PTS-STARTPTS` → применение (`atrim`/`adelay`) → `asetpts=PTS-STARTPTS`; аудио перекодируется в `aac -b:a 256k`. Исследование v1 (без нормализаций) более НЕ использовать.
- Оффлайн-индекс: контракт не меняется (audio-only и marked-set файлы мимо индекса — как в PR165; legacy-путь индексируется как сегодня, sync на форму EntryInput не влияет).
- Новая зависимость gonum разрешена владельцем (research §3.1); после добавления — `make notices` и сверка `THIRD-PARTY-NOTICES.md` (gonum: BSD-3-Clause).
- Язык пользовательских строк: i18n ключи в `locales/en.toml` (истина) и `locales/ru.toml` (дефолт).
- План предполагает исполнителя без контекста исследования: каждый таск самодостаточен.

---

## 1. Архитектура решения (обзор)

```text
TUI «Скачать серии» (startRangeDownload)
  ├─ config download.audio_sync: ask → промпт «Синхронизировать дубляжи?» (1 раз на батч,
  │   только video_audio и merged-сессия >1 провайдера); yes/no → без промпта
  └─ DownloadTask{... AudioSync bool}  ← решение промпта/конфига едет НА ЗАДАЧЕ
        │
        ▼
realCore.downloadOne (единственный хук; общий для foreground и background)
  ├─ resolve video/audio (как сегодня)
  ├─ eligible = треки (или отдельное аудио legacy) с провайдером ≠ video-провайдера
  ├─ ref := analyzer.Reference(video)          // 3×60 c встроенной дорожки, 1 раз на эпизод
  │     └─ ErrNoReference → fallback: первый eligible-дубляж как референс (если их ≥2)
  ├─ для каждого eligible: off := analyzer.Offset(ref, dub)   // ≤15 c, 3 окна
  │     └─ SyncOffset{} / ErrNoConfidence → без сдвига + note
  ├─ AudioTrack.Sync / CommandInput.AudioSync ← SyncOffset
  ├─ c.dl.Download(input)
  │     ├─ прямой источник (не .m3u8): copy-путь: -ss X / -itsoffset X перед СВОИМ -i, -c:a copy
  │     ├─ HLS (.m3u8): фильтр-путь владельца v2: normalize→atrim/adelay→normalize→aresample, aac 256k
  │     └─ ошибка copy-пута при наличии сдвига → ОДИН ретрай через фильтр-путь v2
  └─ отчёт: строки «синхронизация: DUB +17.042с» / «нет уверенности» (foreground);
     background — diagnostics-лог (см. OWNER QUESTIONS #3)
```

Матрица применения (source-type detection по URL):

| Источник дубляжа | offset = 0 / нет уверенности | offset > 0 | offset < 0 |
|---|---|---|---|
| прямой (mp4/mp3/…) | без флагов (как сегодня) | `-ss {offset}` перед `-i` + `-c:a copy` | `-itsoffset {\|offset\|}` перед `-i` + `-c:a copy` |
| HLS (`.m3u8`) | без флагов | фильтр v2: `asetpts=PTS-STARTPTS,atrim=start={offset},asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0` → `aac 256k` | фильтр v2: `asetpts=PTS-STARTPTS,adelay={ms}:all=1,asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0` → `aac 256k` |

Примечания к матрице:

- **HLS по умолчанию — фильтр-путь** (ADDENDUM: у HLS-донора PTS не от нуля; v1-рецепты без нормализации давали паразитную тишину). Task 6 (проб) может вернуть HLS на copy-путь, если чисто — см. политику флипа там.
- `adelay={ms}:all=1`: `all=1` **обязателен** для >2 каналов (research §5.1, измерено) — форма владельца `adelay=ms|ms` (только стерео) заменяется на `all=1`.
- `aresample=async=1:first_pts=0` оставлен финальной стадией (research §5.3: «сохранять как есть»; нормализует PTS-старт, измерено) — при том что v2-эскиз владельца его не упоминает; см. OWNER QUESTIONS #1.
- Анализ (decode в PCM) PTS-агностичен: `-f s16le pipe:` отдаёт сырые сэмплы, время считаем по индексу сэмпла от старта декода; входной `-ss/-t` на HLS ищет по относительной позиции.
- Цепочки нескольких фильтр-треков склеиваются в ОДИН `-filter_complex` через `;` (ffmpeg принимает один такой флаг).

Ключевые константы алгоритма (все — из research, вынесены в `internal/audsync/audsync.go` как экспортируемые калибровочные константы):

| Константа | Значение | Смысл (research §) |
|---|---|---|
| `SampleRate` | 8000 | моно s16le PCM (§3.4) |
| `ReferenceDur` | 60.0 c | референс-окно (§4) |
| `SearchDur` | 180.0 c | поисковое окно (±60 c запаса) |
| `WindowCount` | 3 | окна на пару (§4) |
| `MinProminence` | 20.0 | пик/mean&#124;corr&#124; (§2.3: связанные ≈700–900, несвязанные ≈8) |
| `ConsensusTolerance` | 0.080 c | согласие окон (§4) |
| `SnapTolerance` | 0.020 c | допуск привязки к кадру (§5.4) |
| `DefaultFPS` | 24.0 | при нечитаемом r_frame_rate (§5.4) |
| `MinEpisodeDur` | 300.0 c | короче — анализ не запускать (окна не помещаются) |
| `MinRMS` | 100.0 (s16, ≈ −50 dBFS) | тишина/пустое окно → не голосует (§2.4; калибровочное) |
| `ConfidenceCap` | 10.0 | cap confidence (§4; калибровочное) |
| `DefaultBudget` | 15 s | бюджет пары (§4: ~10–15 c) |

Расписание окон при длительности донора D (§4): старты референса `t₁ = 90`, `t₂ = D/2`, `t₃ = 2D/3`; референс декодируется `[t, t+60]` (у видео), поиск — `[max(0, t−60), 180]` (у дубляжа). Rebasing лага: `offset_samples = D_lag − len(ref)`, `offset_sec = offset_samples / 8000`, диапазон ±60 c.

---

### Task 1: пакет `internal/audsync` — типы, константы, чистая корреляция (gonum)

**Files:**

- Create: `internal/audsync/audsync.go`
- Create: `internal/audsync/correlate.go`
- Test: `internal/audsync/correlate_test.go`
- Modify: `go.mod` / `go.sum` (gonum), `THIRD-PARTY-NOTICES.md`

**Interfaces (produces):**

```go
package audsync

// SyncOffset — вердикт анализа одной пары (видео, дубляж). Нулевое
// значение = «нет уверенности» → mux без сдвига (research §6.3).
type SyncOffset struct {
    Seconds      float64 // сдвиг донора, c; >0 = обрезать начало донора, <0 = долить тишину
    Confidence   float64 // min(prominence)/MinProminence, cap ConfidenceCap; ≥1 у принятых
    WindowsAgree int     // согласных окон из годных (2 или 3)
}

var (
    ErrNoConfidence = errors.New("audsync: no confident offset") // анализ не дал вердикта (НЕ ошибка загрузки)
    ErrNoReference  = errors.New("audsync: reference track is silent or absent")
)

const ( /* таблица констант из раздела «Архитектура» — переносим дословно */ )
```

```go
// correlate.go — research §3.3, рецепт проверен:
// c = IFFT( FFT(x) · conj(FFT(y)) ), N = next_pow2(len(x)+len(y)−1), оба добиты нулями;
// k = argmax(seq); D = (N−k) mod N — задержка srch относительно ref (srch[i] = ref[i−D]).
func nextPow2(n int) int
func CrossCorrelate(ref, srch []float64) (delay int, prominence float64, err error)
```

- [ ] **Step 1: зависимость.** `export TMPDIR=... ; go get gonum.org/v1/gonum@v0.17.0 && go mod tidy`. `make notices` → руками добавить строку gonum (BSD-3-Clause) в `THIRD-PARTY-NOTICES.md`.

- [ ] **Step 2: failing test корреляции (синтетика, без ffmpeg).**

```go
func TestCrossCorrelateFindsExactDelay(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	x := make([]float64, 48000) // 6 c @8k
	for i := range x {
		x[i] = math.Sin(2*math.Pi*7.3*float64(i)/8000) + 0.4*math.Sin(2*math.Pi*61*float64(i)/8000) + rng.NormFloat64()*0.05
	}
	for _, tc := range []struct{ name string; delay int }{
		{"small", 12345}, {"large", 98765},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srch := make([]float64, 144000)
			for i := tc.delay; i < len(srch); i++ {
				srch[i] = x[i-tc.delay] // srch[i] = ref[i−D]
			}
			d, prom, err := CrossCorrelate(x, srch)
			if err != nil { t.Fatalf("err: %v", err) }
			if d != tc.delay { t.Fatalf("delay = %d, want %d", d, tc.delay) }
			if prom < MinProminence { t.Fatalf("prominence = %v, want ≥ %v", prom, MinProminence) }
		})
	}
}

func TestCrossCorrelateUncorrelatedLowProminence(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	a := make([]float64, 48000); b := make([]float64, 144000)
	for i := range a { a[i] = rng.NormFloat64() }
	for i := range b { b[i] = rng.NormFloat64() }
	_, prom, err := CrossCorrelate(a, b)
	if err != nil { t.Fatalf("err: %v", err) }
	if prom >= MinProminence { t.Fatalf("prominence = %v, want < %v (несвязанные ≈ 8)", prom, MinProminence) }
}

func TestNextPow2(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{1, 1}, {2, 2}, {3, 4}, {191999, 262144}, {262144, 262144}} {
		if got := nextPow2(tc.in); got != tc.want {
			t.Fatalf("nextPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 3:** запустить `go test -race -count=1 ./internal/audsync/ -run TestCrossCorrelate -v` → FAIL (CrossCorrelate undefined).
- [ ] **Step 4: минимальная реализация** — `audsync.go` (package doc: «аудио-автосинк дубляжей, research .sdd/pr165b-me-sync-research.md»; типы, ошибки, константы) и `correlate.go`:

```go
import "gonum.org/v1/gonum/dsp/fourier"

func nextPow2(n int) int {
	p := 1
	for p < n { p <<= 1 }
	return p
}

func CrossCorrelate(ref, srch []float64) (int, float64, error) {
	if len(ref) == 0 || len(srch) == 0 {
		return 0, 0, errors.New("audsync: empty signal")
	}
	n := nextPow2(len(ref) + len(srch) - 1)
	px := make([]float64, n); copy(px, ref)
	py := make([]float64, n); copy(py, srch)
	fft := fourier.NewFFT(n)
	x := fft.Coefficients(nil, px)
	y := fft.Coefficients(nil, py)
	for i := range x { x[i] *= cmplx.Conj(y[i]) }
	seq := fft.Sequence(nil, x)
	k := 0
	sum := 0.0
	for i, v := range seq {
		av := math.Abs(v)
		sum += av
		if v > seq[k] { k = i }
	}
	return (n - k) % n, seq[k] / (sum / float64(n)), nil
}
```

- [ ] **Step 5:** тесты из Step 2 — PASS. Память/время: N=2¹⁸, ~0.4 c/окно (research §3.2, не проверяем здесь — это уже измерено).
- [ ] **Step 6: gate + commit** `feat(audsync): FFT cross-correlation core on gonum (PR165b T1)`.

---

### Task 2: `internal/audsync` — окна, вердикт (консенсус/медиана/confidence), snap

**Files:**

- Create: `internal/audsync/windows.go`
- Create: `internal/audsync/analyze.go` (только чистая часть: `analyzePair` + `snapToFrame`)
- Test: `internal/audsync/analyze_test.go`

**Interfaces:**

- Consumes: `CrossCorrelate`, константы (Task 1), `SyncOffset`.
- Produces:

```go
// windows.go
type Window struct { RefStart, SearchStart float64 } // секунды
// WindowSchedule: t = 90, D/2, 2D/3; nil если D < MinEpisodeDur (анализ неприменим).
func WindowSchedule(dur float64) []Window

// analyze.go — ЧИСТАЯ функция (без exec): на вход уже декодированные окна.
// refWins[i] — референс [t, t+60]; searchWins[i] — поиск [max(0,t−60), ≤180]; fps — сетка кадров.
// Правило вердикта (research §4): окно годно, если RMS(ref) ≥ MinRMS && RMS(srch) ≥ MinRMS &&
// prominence ≥ MinProminence; offset_i = (delay_i − len(ref_i))/SampleRate;
// ≥2 годных согласны в ±ConsensusTolerance → медиана → snapToFrame → SyncOffset; иначе ErrNoConfidence.
func analyzePair(refWins, searchWins [][]float64, fps float64) (SyncOffset, error)
func snapToFrame(off, fps float64) float64 // round(off*fps)/fps при |остатке| ≤ SnapTolerance, иначе off
func rms(x []float64) float64
```

- [ ] **Step 1: failing tests (синтетика — главный контракт знака).**

```go
// Генератор «дубляжа»: референс, сдвинутый вправо на K секунд (+ шум +-dialog-имитация
// (40% сэмплов одной стороны занулено — research §2.3: корреляция выживает)).
func shiftSignal(x []float64, kSec float64, sr float64) []float64 {
	k := int(kSec * sr)
	out := make([]float64, len(x)+k+int(60*sr)) // добор до поисковой длины
	copy(out[k:], x)
	return out
}

func TestAnalyzePairFindsShiftedDubExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, k := range []float64{17.042, -0.958, 6.25, 0.5} {
		ref := wobble(60 * 8000, rng)               // сумма синусов + шум (см. Task 1 Step 2)
		refWin := []refWindowAt(t0=90c)             // нарезка: 3 референс-окна по [t,t+60] из «видео»
		video := longSignal(ref, 24*60)             // видеотаймлайн 24 мин: референс с t=90
		dub := shiftVideo(video, k)                 // дубль-таймлайн = видео, сдвинутое на k
		refWins, searchWins := cutWindows(video, dub, 300.0 /*dur*/) // по WindowSchedule
		off, err := analyzePair(refWins, searchWins, 24)
		if err != nil { t.Fatalf("k=%v: %v", k, err) }
		if math.Abs(off.Seconds-k) > 0.05 { t.Fatalf("k=%v: got %v", k, off.Seconds) }
		if off.WindowsAgree < 2 { t.Fatalf("k=%v: agree=%d", k, off.WindowsAgree) }
		if off.Confidence < 1 { t.Fatalf("k=%v: confidence=%v", k, off.Confidence) }
		// snap: 17.042 ≈ 409/24 — остаётся; 6.25 = 150/24 — остаётся
	}
}

func TestAnalyzePairNoConfidenceOnSilence(t *testing.T) {
	refWins := [][]float64{zeros(60*8000), zeros(60*8000), zeros(60*8000)}
	searchWins := [][]float64{noise(180*8000), noise(180*8000), noise(180*8000)}
	if _, err := analyzePair(refWins, searchWins, 24); !errors.Is(err, ErrNoConfidence) {
		t.Fatalf("want ErrNoConfidence, got %v", err)
	}
}

func TestAnalyzePairConsensusIgnoresDialogueOnlyWindow(t *testing.T) {
	// Окна 1 и 3: чистый сдвиг k=+2.0 (shiftSignal из TestAnalyzePairFindsShiftedDubExactly,
	// ref-окно 60 c против поискового 180 c). Окно 2 — «диалог-only»: несвязанный шум
	// (rng-генератор с другим seed) → prominence < MinProminence → окно не голосует.
	// Вердикт: +2.0, WindowsAgree=2; err == nil.
}

func TestAnalyzePairMedianOfThree(t *testing.T) {
	// Три годных окна (все prominence ≥ MinProminence) со сдвигами 2.000 / 2.004 / 2.080:
	// 2.080 отходит от 2.004 на 76 мс ≤ ConsensusTolerance → все три согласны →
	// медиана = 2.004 (сортировка, средний элемент). Assert off.Seconds == 2.004 ± 1e-9.
}

func TestSnapToFrame(t *testing.T) {
	cases := []struct{ name string; in, fps, want float64 }{
		{"snaps_within_tolerance", 17.050, 24, 17.041666}, // остаток 8.3 мс ≤ 20 мс
		{"already_on_grid", 6.250, 24, 6.250},             // 150/24 точно
		{"keeps_off_grid", 17.100, 24, 17.100},            // остаток 58 мс > 20 мс
		{"default_fps", 2.000, 0, 2.000},                  // fps=0 → DefaultFPS=24, 48/24 на сетке
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fps := tc.fps
			if fps == 0 { fps = DefaultFPS }
			if got := snapToFrame(tc.in, fps); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("snapToFrame(%v, %v) = %v, want %v", tc.in, fps, got, tc.want)
			}
		})
	}
}

func TestWindowSchedule(t *testing.T) {
	// D=1440: {90, 720, 960}; D=299 → nil; D=301 → 3 окна, все start+60 ≤ D−1 (t₃ = 2D/3).
}
```

- [ ] **Step 2:** `go test -race ./internal/audsync/ -run 'TestAnalyzePair|TestSnap|TestWindowSchedule' -v` → FAIL (undefined).
- [ ] **Step 3: реализация** `windows.go` + `analyze.go` (только чистые функции; orchestration с exec — Task 3). RMS-гейт ДО корреляции; медиана для 2 окон — среднее; `confidence = min(prom agrees)/MinProminence`, cap `ConfidenceCap`.
- [ ] **Step 4:** тесты PASS. **Step 5: gate + commit** `feat(audsync): window schedule, consensus verdict, frame snap (PR165b T2)`.

---

### Task 3: `internal/audsync` — exec-слой: ffmpeg-pipe PCM, ffprobe-пробы, `Reference`/`Offset`

**Files:**

- Create: `internal/audsync/exec.go`
- Create: `internal/audsync/pcm.go`
- Create: `internal/audsync/probe.go`
- Modify: `internal/audsync/analyze.go` (orchestration)
- Test: `internal/audsync/pcm_test.go`, `internal/audsync/probe_test.go`, `internal/audsync/offset_test.go`

**Interfaces:**

```go
// exec.go — инъекционный шов (аналог download.runner): stdout буферизуется целиком
// (180 c × 8000 × 2 байта ≈ 2.9 MB — копейки).
type execFunc func(ctx context.Context, bin string, args []string) (stdout []byte, stderr string, err error)
func realExec(ctx context.Context, bin string, args []string) ([]byte, string, error) // exec.CommandContext + cmd.Output()

// audsync.go
type Options struct {
	FFmpeg  string        // дефолт "ffmpeg"
	FFprobe string        // дефолт: sibling от FFmpeg (как download.probeBin, 169-K), иначе "ffprobe"
	Budget  time.Duration // дефолт DefaultBudget (15 c) — НА ПАРУ (Offset)
	Exec    execFunc      // nil = realExec
}
func New(opts Options) *Analyzer

// analyze.go
// Reference — окна встроенной аудиодорожки видео (декод ОДИН раз на эпизод, research §6.2),
// плюс fps видео для snap. Все окна тише MinRMS → ErrNoReference.
type Reference struct { Windows [][]float64; FPS float64 }
func (a *Analyzer) Reference(ctx context.Context, video contracts.VideoSource) (Reference, error)

// Offset — вердикт для одного дубляжа против готового Reference: проба длительности донора
// (format=duration) → расписание окон → декод 3×поисковых окон у донора → analyzePair.
// Бюджет: context.WithTimeout(Budget) на весь вызов (убивает ffmpeg по истечении).
// ЛЮБАЯ ошибка (probe/decode/timeout/ErrNoConfidence) → (SyncOffset{}, ErrNoConfidence-wrapped)
// — сигнатура (SyncOffset, error), ошибки типа ErrNoConfidence/ErrNoReference/обёртки.
func (a *Analyzer) Offset(ctx context.Context, ref Reference, dub contracts.VideoSource) (SyncOffset, error)
```

argv декода (research §3.4, дословно; заголовки — единым `-headers`-блобом как `headerArgs` в download; ресемплинг в моно 8 кГц делает сам ffmpeg флагами `-ac 1 -ar 8000` — отдельного Go-ресемплера в пакете НЕТ и не нужен):

```text
ffmpeg -hide_banner -loglevel error -nostdin [-headers "<CRLF-joined>"] -ss {start} -t {dur} -i {URL} -vn -ac 1 -ar 8000 -f s16le pipe:
```

argv пробы длительности: `ffprobe -v error -show_entries format=duration -of json {URL}` (+headers).
argv пробы fps: `ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of json {URL}` (+headers); парс `24000/1001` → 23.976…; нечитаемо → `DefaultFPS`.

- [ ] **Step 1: failing test argv + парсинга (fake exec, без процессов).**

```go
func TestDecodeArgs(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/v.m3u8", Headers: map[string]string{"Referer": "https://r/"}}
	got := decodeArgs(src, 90, 180)
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-headers", "Referer: https://r/\r\n", "-ss", "90.000", "-t", "180.000",
		"-i", "https://x/v.m3u8", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:"}
	if !reflect.DeepEqual(got, want) { t.Fatalf("got %q", got) }
}

func TestDecodePCMConvertsS16LE(t *testing.T) {
	// fake exec отдаёт 8 сэмплов little-endian s16 → []float64{...}; отрицательные/кратные 32768.
}

func TestParseDurationAndFPS(t *testing.T) {
	// duration: {"format":{"duration":"1440.5"}} → 1440.5; мусор/пусто → error (пара не анализируется).
	// fps: "24000/1001" → 23.976…; "24/1" → 24; мусор/ошибка пробы → DefaultFPS (НЕ ошибка анализа —
	// деградация; решение snap лишь слегка грубее).
}

func TestOffsetBudgetKillsDecode(t *testing.T) {
	// fake exec спит > Budget → Offset вернёт ErrNoConfidence-обёртку за ≤ Budget+ε.
}

func TestReferenceErrOnSilence(t *testing.T) {
	// fake exec отдаёт нули → ErrNoReference.
}
```

- [ ] **Step 2:** FAIL. **Step 3: реализация** `exec.go`, `pcm.go` (байты → `binary.LittleEndian` → int16 → float64/32768), `probe.go`, orchestration в `analyze.go`:
  - `Offset`: `withTimeout` → `probeDuration(dub)` (fail → обёртка ErrNoConfidence c причиной в лог-строке вызова? — нет: Analyzer без логгера; причина возвращается в error-обёртке) → `WindowSchedule(dur)` (nil → ErrNoConfidence) → цикл окон: `decodePCM(dub, max(0,t−60), SearchDur)`; ref-окна уже в `ref.Windows` → `analyzePair`.
  - `Reference`: fps-проба + 3 декода `[t, t+60]` по расписанию от dur ВИДЕО (probeDuration(video); fail → расписание от константы 1440 c? — НЕТ: без dur окна не построить; fail пробы видео = ErrNoReference-обёртка). Все окна RMS < MinRMS → ErrNoReference.
- [ ] **Step 4:** PASS. **Step 5: gate + commit** `feat(audsync): ffmpeg PCM decode, ffprobe duration/fps, Reference/Offset orchestration (PR165b T3)`.

---

### Task 4: конфиг `download.audio_sync` + i18n

**Files:**

- Modify: `internal/config/config.go` (структура `Download` ~строка 118; дефолты ~строка 411)
- Modify: `settings.example.toml` (блок `[download]`, ~строка 152)
- Modify: `locales/en.toml`, `locales/ru.toml`
- Test: `internal/config/config_test.go`

**Interfaces:**

```go
type Download struct {
	MaxConcurrency int `toml:"max_concurrency"`
	// AudioSync управляет автосинхронизацией дубляжей («кросс-корреляция
	// M&E»): "ask" — промпт один раз на батч (дефолт), "yes" — всегда
	// анализировать, "no" — никогда. Неизвестное значение деградирует в
	// "ask" при загрузке (плюс строка в лог) — конфиг-опечатка не брюшит
	// старт TUI.
	AudioSync string `toml:"audio_sync"`
}
```

i18n-ключи (новые, добавить в ОБА файла):

```toml
"download.sync_ask_title" = "Синхронизировать дубляжи по видео? (анализ ~15 с на пару)"   # ru; en-зеркало
"download.sync_choice_yes" = "✓ Да — анализировать и править"
"download.sync_choice_no" = "✗ Нет — как есть"
"download.row_sync" = "синхронизация: {notes}"
"download.sync_note_applied" = "{dub} {offset}с"
"download.sync_note_uncertain" = "{dub}: нет уверенности"
"download.sync_verdict" = "анализ синхронизации…"   # прогресс-строка анализа
```

- [ ] **Step 1: failing test** (`config_test.go`): парс `audio_sync = "yes"` → поле "yes"; `audio_sync = "мусор"` → нормализация в "ask"; отсутствие ключа → дефолт "ask".
- [ ] **Step 2:** FAIL. **Step 3:** поле + дефолт в конструкторе дефолтов + `normalizeAudioSync(s string) string` в load-пути; ключи в оба toml; `settings.example.toml`:

```toml
[download]
# ...
# Автосинхронизация дубляжей (кросс-корреляция M&E): ask — спросить
# перед батчем (по умолчанию), yes — всегда, no — никогда.
# audio_sync = "ask"
```

- [ ] **Step 4:** PASS. **Step 5: gate + commit** `feat(config): download.audio_sync ask|yes|no (PR165b T4)`.

---

### Task 5: применение сдвига в `internal/download` — copy-путь + фильтр-путь владельца v2, isHLS

**Files:**

- Modify: `internal/download/mux.go` (AudioTrack, билдеры)
- Modify: `internal/download/downloader.go` (CommandInput)
- Test: `internal/download/mux_test.go` (argv-golden)

**Interfaces:**

```go
// mux.go
type AudioTrack struct {
	// ...существующие поля...
	// Sync — вердикт аудио-анализа (nil = без сдвига; Seconds с |x|<0.001
	// флагов не порождает). Путь применения выбирается по типу источника:
	// isHLS → фильтр v2, иначе copy.
	Sync *audsync.SyncOffset
}

// downloader.go
type CommandInput struct {
	// ...существующие поля...
	// AudioSync — сдвиг для ОТДЕЛЬНОГО аудио legacy-пути (input.Audio);
	// встроенное аудио (Audio == nil) не сдвигается по определению.
	AudioSync *audsync.SyncOffset
	// SyncFilterOverride — внутренний рычаг ретрая: заставить ВСЕ сдвиги
	// идти фильтр-путём v2 (copy-путь не сработал). Ставит только ретрай
	// в tui; вручную не выставлять.
	SyncFilterOverride bool
}

func isHLS(rawURL string) bool // срезать query/fragment, ToLower, HasSuffix ".m3u8"
// syncPerInputArgs: offset>0 → ["-ss", "17.042"]; offset<0 → ["-itsoffset", "0.958"]; |x|<0.001 → nil.
func syncPerInputArgs(off float64) []string
// filterSyncChain — рецепт владельца v2 (ADDENDUM), inputIdx — индекс -i:
// offset>0: "[N:a:0]asetpts=PTS-STARTPTS,atrim=start={off},asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0[aN]"
// offset<0: "[N:a:0]asetpts=PTS-STARTPTS,adelay={ms}:all=1,asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0[aN]"
func filterSyncChain(inputIdx int, off float64) string
```

Формат секунд: `strconv.FormatFloat(x, 'f', 3, 64)`; `ms = int64(math.Round(math.Abs(off)*1000))`.

Изменения билдеров (показано на `buildMuxArgs`; `buildVideoOnlyArgs`/`buildAudioOnlyArgs` не трогаются — им сдвиг не положен):

```go
// 1) ВХОДЫ: per-input sync строго перед СВОИМ -i (после headerArgs):
for i, tr := range tracks {
	args = append(args, headerArgs(tr.Source.Headers)...)
	if tr.Sync != nil && !useFilterPath(tr) {
		args = append(args, syncPerInputArgs(tr.Sync.Seconds)...)
	}
	args = append(args, "-i", tr.Source.URL)
}
// 2) ФИЛЬТР-ТРЕКИ: одна цепочка -filter_complex, мапы [aN] вместо N:a:0,
//    per-stream кодек: -c:a copy глобально + "-c:a:N","aac","-b:a:N","256k" для фильтр-треков.
//    Порядок -map сохраняется (видео, потом аудио по индексам треков) —
//    disposition/metadata циклы остаются без изменений.
// 3) useFilterPath(tr) = in.SyncFilterOverride || isHLS(tr.Source.URL)
```

Legacy-ветка `BuildCommand` (default): если `in.Audio != nil && in.AudioSync != nil` — те же два режима: copy → `syncPerInputArgs` перед `-i audio`; фильтр → `-filter_complex "[1:a:0]{v2chain}[a1]"`, `-map 0:v -map [a1]`, `-c:v copy -c:a aac -b:a 256k` (одно аудио — глобальные кодеки).

- [ ] **Step 1: failing argv-golden тесты.**

```go
func TestMuxCopyPathPositiveOffset(t *testing.T) {
	in := CommandInput{Video: vid, Mode: ModeVideoAudio, OutputPath: "o.mkv", Title: "t"}
	tracks := []AudioTrack{{Source: directSrc, Default: true, Sync: &audsync.SyncOffset{Seconds: 17.042, Confidence: 3, WindowsAgree: 3}}}
	args := buildMuxArgs(in, tracks)
	// golden: ... "-headers", ..., "-ss", "17.042", "-i", directSrc.URL, ... "-map","0:v:0","-map","1:a:0","-c:v","copy","-c:a","copy", ... disposal/meta как сегодня
}

func TestMuxCopyPathNegativeOffset(t *testing.T) { /* "-itsoffset","0.958" перед -i дубляжа */ }

func TestMuxFilterPathV2HLS(t *testing.T) {
	// Source.URL = "https://x/audio.m3u8?token=1" → isHLS true:
	// "-filter_complex", "[1:a:0]asetpts=PTS-STARTPTS,atrim=start=17.042,asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0[a1]",
	// "-map","0:v:0","-map","[a1]","-c:v","copy","-c:a","copy","-c:a:0","aac","-b:a:0","256k"
	// и НИКАКИХ -ss/-itsoffset.
}

func TestMuxFilterPathV2NegativeAdelay(t *testing.T) { /* adelay=958:all=1 */ }

func TestMuxMixedCopyAndFilter(t *testing.T) {
	// трек 0 прямой (copy, -ss), трек 1 HLS (фильтр [a2]): оба -map корректны,
	// один -filter_complex из одной цепочки (цепочка только у трека 1).
}

func TestMuxZeroOffsetNoFlags(t *testing.T) { /* Sync{Seconds: 0.0004} → argv идентичен сегодняшнему */ }

func TestLegacySeparateAudioSyncCopyAndFilter(t *testing.T) { /* CommandInput.AudioSync: оба режима */ }

func TestIsHLSTable(t *testing.T) {
	// "http://x/a.m3u8"→true; ".../index.m3u8?tok=1#frag"→true; ".../A.M3U8"→true;
	// "http://x/a.mp4"→false; "http://x/m3u8.foo"→false.
}
```

- [ ] **Step 2:** FAIL. **Step 3:** реализация по эскизу выше. **Step 4:** PASS (весь `go test -race ./internal/download/`).
- [ ] **Step 5: gate + commit** `feat(download): sync offset application — copy path, owner v2 filter recipe, isHLS (PR165b T5)`.

---

### Task 6: ВЕРИФИКАЦИОННЫЙ ПРОБ V1 — HLS PTS и выбор copy vs filter

**Files:**

- Create (временные фиксачи в `$TMPDIR/audsync-probe/`, команды ниже — дословно)
- Create: `.sdd/pr165b-me-sync-verify.md` (записанные результаты пробы)

Это **исполнимый верификационный шаг** плана (не код): добавить в пакет только если проб изменит матрицу (флип). Вопрос ADDENDUM: ведёт ли copy-путь (`-ss`/`-itsoffset` + `-c:a copy`) себя чисто на HLS с ненулевой базой PTS (без паразитной тишины/ошибок сдвига)?

**Фиксачи** (tone 1 кГц на [5..35] c из 40 c, база PTS +3600 c):

```bash
export TMPDIR=/home/whoami/.cache/go-tmp; P=$TMPDIR/audsync-probe; mkdir -p $P; cd $P
FF='ffmpeg -hide_banner -loglevel error -y'
# donor: 5s тишина + 30s тон + 5s тишина, stereo aac
$FF -f lavfi -i "sine=frequency=1000:duration=30" -f lavfi -i "anullsrc=r=44100:cl=stereo" \
  -filter_complex "[0:a]aformat=sample_rates=44100:channel_layouts=stereo[t];[1:a]atrim=0:5[s1];[1:a]atrim=0:5[s2];[s1][t][s2]concat=n=3:v=0:a=1[a]" \
  -map "[a]" -c:a aac -b:a 192k donor.m4a
# видео-подложка 40 c
$FF -f lavfi -i "testsrc=duration=40:size=320x240:rate=24" -c:v libx264 -pix_fmt yuv420p vid.mp4
# сдвинутая база PTS +3600: прямой mp4 и HLS
$FF -i donor.m4a -c copy -output_ts_offset 3600 donor_direct.mp4
$FF -i donor.m4a -c copy -output_ts_offset 3600 -f hls -hls_time 4 -hls_list_size 0 donor_hls.m3u8
```

**Пробы** (mux видео+донор, меряем `ffprobe start_time` + `silencedetect`):

```bash
# PROBE A — copy-путь, offset=0, HLS-донор с базой 3600: есть ли ПАРАЗИТНАЯ тишина?
ffmpeg -hide_banner -loglevel error -y -i vid.mp4 -i "donor_hls.m3u8" -map 0:v:0 -map 1:a:0 -c copy outA.mkv
ffprobe -v error -select_streams a:0 -show_entries stream=start_time -of csv=p=0 outA.mkv
ffmpeg -hide_banner -i outA.mkv -af silencedetect=noise=-50dB:d=0.2 -f null - 2>&1 | grep silence_

# PROBE B — фильтр v2 (владелец), offset=+3, HLS: регрессионная защита рецепта
ffmpeg ... -i vid.mp4 -i donor_hls.m3u8 -map 0:v:0 -filter_complex "[1:a:0]asetpts=PTS-STARTPTS,atrim=start=3,asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0[a1]" -map "[a1]" -c:v copy -c:a aac -b:a 256k outB.mkv
# (те же два замера)

# PROBE C — copy-путь, offset=+3 (-ss 3 перед -i HLS): доказательство для возможного флипа
ffmpeg ... -i vid.mp4 -ss 3 -i donor_hls.m3u8 -map 0:v:0 -map 1:a:0 -c copy outC.mkv
# (те же два замера)
```

**Ожидания-ассерты:**

- A: `start_time` аудио |x| ≤ 0.05; первый `silence_end` ≈ 5.0 ± 0.3 (собственная вводная тишина фикcачи; паразитная тишина дала бы silence_end ≫ 5 или start_time ≫ 0).
- B: первый `silence_end` ≈ 2.0 ± 0.3 (срезаны 3 из 5 c тишины), `start_time` ≈ 0, хвост сохранён.
- C: как B, но copy-путём — это и есть вопрос флипа.

**Политика флипа:** если C чист (оба ассерта в допуске) — исполнитель МОЖЕТ в этом же PR переключить HLS-ветку `useFilterPath` на copy (одно место в `mux.go` + правка матрицы в этом документе + результаты в `.sdd/pr165b-me-sync-verify.md`). Если C грязен или спорен — HLS остаётся на фильтр v2 (валидировано владельцем), flip только после OWNER QUESTIONS #2. Пробы A/B фиксируются в любом случае.

- [ ] **Step 1:** выполнить фикcачи и пробы A, B, C; записать сырой вывод в `.sdd/pr165b-me-sync-verify.md`.
- [ ] **Step 2:** применить политику флипа; коммит при флипе: `feat(download): verified HLS copy-path sync per probe V1 (PR165b T6)`; иначе документ-коммит `docs: record HLS sync probe V1 results (PR165b T6)`.

---

### Task 7: интеграция в `downloadOne` — анализ, plumbing, ретрай, отчёт

**Files:**

- Modify: `internal/tui/real.go` (realCore: поле analyzer; `downloadOne`; `realDownload.Download`)
- Modify: `internal/tui/services.go` (DownloadService.Download сигнатура; DownloadTask.AudioSync)
- Modify: `internal/tui/download_flow.go` (runForegroundDownload: прокидка sync-заметок; рендер суффикса)
- Test: `internal/tui/pr165b_me_sync_test.go`

**Interfaces:**

```go
// services.go
type DownloadTask struct {
	// ...существующие поля...
	// AudioSync — решение «анализировать ли пары» для этой задачи
	// (конфиг + промпт, проставляется на батче в startRangeDownload).
	AudioSync bool
}

type DownloadService interface {
	// Download возвращает также sync-заметки пар этой серии
	// (пусто = анализ не запускался/ничего не нашёл).
	Download(ctx context.Context, task DownloadTask) (path string, dropped []string, sync []SyncNote, err error)
	// ...
}

// download_flow.go — заметка одной пары для строки отчёта.
type SyncNote struct {
	Dub      string  // имя дубляжа (без provider-тега)
	Seconds  float64
	Applied  bool    // false = анализ шёл, уверенности нет
}

// real.go — шов анализатора (по образцу dl/prober); nil = фича выключена
// (сегодняшнее поведение; тесты подменяют фейком).
type audioSyncAnalyzer interface {
	Reference(ctx context.Context, video contracts.VideoSource) (audsync.Reference, error)
	Offset(ctx context.Context, ref audsync.Reference, dub contracts.VideoSource) (audsync.SyncOffset, error)
}
// продакшн-сборка: real.analyzer = audsync.New(audsync.Options{FFmpeg: "ffmpeg"})
```

Порядок в `downloadOne` (после resolve video/tracks, до `ensurePathWithinRoot`/`c.dl.Download`):

1. Gate: `task.AudioSync && c.analyzer != nil`, иначе — как сегодня.
2. Сбор eligible-пар: (a) marked-set: для каждого `track` из `resolveAudioTracks` — `providerOfTrackKey(оригинальный key) != task.ProviderID` (соответствие key↔track держим индексом цикла); (b) legacy: `input.Audio != nil` и провайдер отдельного аудио ≠ `task.ProviderID` → одна пара с `AudioSync`.
3. Референс-цепочка (research §6.2): `ref, err := c.analyzer.Reference(ctx, video)`; `errors.Is(err, audsync.ErrNoReference)` и eligible ≥ 2 → `ref = Reference(первый eligible-дубляж)` (его окна — референс для остальных; при этом сам первый в анализе не участвует); иначе ErrNoReference и eligible < 2 → skip с warn-логом.
4. Для каждого eligible: `off, err := c.analyzer.Offset(ctx, ref, track.Source)`.
   - `err != nil` (timeout/no-confidence/прочее) → `SyncNote{Dub, Applied:false}` + warn-лог с причиной из обёртки.
   - Успех с |off.Seconds| < 0.001 (уверенный ноль после snap — пара уже синхронна) → БЕЗ SyncNote и без флагов, info-лог «offset 0 (already in sync)».
   - Иначе → `track.Sync = &off` / `input.AudioSync = &off`, `SyncNote{Dub, Seconds, Applied:true}` + info-лог (`offset, confidence, путь copy|filter`). Dub-метка: имя трека без provider-тега (`stripProviderTag`); для legacy-пары — `stripProviderTag(audioKey)`.
5. Download + ретрай: `err := c.dl.Download(ctx, input)`; если err != nil И есть хоть один sync-вход на copy-пути И `!input.SyncFilterOverride` → `input.SyncFilterOverride = true` → ровно ОДИН повторный `c.dl.Download`; вторая ошибка → вернуть `fmt.Errorf("...: %w (filter-path retry also failed)", firstErr)`.
6. Вернуть `sync []SyncNote` наверх; индекс/имя файла — без изменений.

Отчёт (foreground): `runForegroundDownload` принимает 4-е возвращаемое; `downloadEpisodeReport.Sync []SyncNote`; в `renderDownloadSettle` после `dropped`-суффикса:

```go
if sfx := renderSyncSuffix(r.Sync); sfx != "" {
	fmt.Fprintf(&b, " (%s)", sfx) // «синхронизация: DUB +17.042с, DUB2: нет уверенности»
}
// renderSyncSuffix: i18n.T("download.row_sync", {notes: strings.Join(notes, ", ")})
// note applied: i18n.T("download.sync_note_applied", {dub, offset: fmt.Sprintf("%+.3f", sec)})
// note uncertain: i18n.T("download.sync_note_uncertain", {dub})
```

Background: заметки в лог (`runDownload` игнорирует возврат — log внутри downloadOne уже пишет всё); строки очереди — без суффикса (см. OWNER QUESTIONS #3).

Прогресс-строка анализа: честный тик `download.sync_verdict` НЕ вводится в v1 (анализ идёт внутри `deps.Download.Download`, между существующими «Загрузка i/N» тиками; менеджерский progress 0→100 не трогаем) — осознанное упрощение, лог покрывает.

- [ ] **Step 1: failing tests.**
  - fake analyzer: `Reference` → 3 окна вобла; `Offset` → заданный `SyncOffset` (в т.ч. ошибка → `Applied:false`).
  - fake `mediaDownloader` (argv-капчер): assert `-ss 17.042` для прямого, `-filter_complex` для HLS, ретрай-последовательность (первый вызов copy → ошибка → второй с `SyncFilterOverride`), отсутствие анализа при `task.AudioSync == false`, отсутствие флагов при provider-равенстве.
  - `runForegroundDownload` + fake `DownloadService`: строка отчёта содержит «синхронизация: DUB +17.042с»; no-confidence — «нет уверенности».
  - offline-index: legacy-путь с sync создаёт запись индекса как сегодня (формат EntryInput не менялся).
- [ ] **Step 2:** FAIL. **Step 3:** реализация по порядку п.1–6 + правка фейка `droppingDownload` (pr165_download_modes_test.go:415) под новую сигнатуру. **Step 4:** PASS весь `go test -race ./internal/tui/`.
- [ ] **Step 5: gate + commit** `feat(tui): audio-sync analysis hook in downloadOne, sync report rows, copy→filter retry (PR165b T7)`.

---

### Task 8: TUI-промпт `ask` + проводка на батч

**Files:**

- Modify: `internal/tui/session.go` (state, меню, startRangeDownload), `internal/tui/app.go` (Deps.DownloadSync)
- Modify: `internal/tui/real.go` (сборка Deps: снапшот настройки)
- Test: `internal/tui/pr165b_sync_ask_test.go`

**Interfaces / поведение:**

```go
// app.go — снапшот настройки для сессий (zero value "" ведёт себя как "ask").
type Deps struct { /* ... */ DownloadSync string }

// session.go — новые поля:
syncAskMode   string // stash mode/strict на время промпта
syncAskStrict bool
// syncChoice *bool — НОВЫЙ параметр startRangeDownload: nil = решение не принято
// (можно спросить), &true/&false = решено (промпт и повторное чтение конфига не нужны).

// Все существующие вызовы (session.go:2856 и :2986) переходят на nil:
//   s.startRangeDownload(mode, strict, nil)

func (s *sessionScreen) startRangeDownload(mode string, strict bool, syncChoice *bool) (Screen, tea.Cmd) {
	content := s.downloadContentMode
	if content == "" { content = "video_audio" }
	if syncChoice == nil {
		cfg := s.deps.DownloadSync // "" | "ask" | "yes" | "no"
		switch {
		case content != "video_audio" || len(s.parts) < 2 || cfg == "no":
			f := false; syncChoice = &f
		case cfg == "yes":
			f = true; syncChoice = &f
		default: // "ask" (и "" как ask): stash вызова и промпт
			s.syncAskMode, s.syncAskStrict = mode, strict
			s.openSyncAskMenu() // PinList: sync_choice_yes / sync_choice_no, форма dub-меню #164
			return s, nil
		}
	}
	ask := *syncChoice
	// ...построение tasks c AudioSync: ask (иначе false) и dispatch
	// foreground/background — без изменений относительно сегодняшнего кода.
}

// handleSyncAskKey: «Да» → choice=true; «Нет» и Esc/Back → choice=false (батч
// продолжается БЕЗ синхронизации — это НЕ отмена загрузки); затем
// return s.startRangeDownload(s.syncAskMode, s.syncAskStrict, &choice).
```

Гейт одноразовый по построению: промпт открывается только при `syncChoice == nil`, а обработчик всегда отвечает явным значением — re-entry loop невозможен. Режимы `video_only`/`audio_only` и одиночная-сессия (`len(s.parts) < 2`) гейт не проходят вовсе.

- [ ] **Step 1: failing tests:** (a) config ask + merged-сессия → задачи с AudioSync true после «Да», false после «Нет»; (b) config yes/no → меню не открывается вовсе; (c) одиночная-сессия (len(parts)==1) → гейт не срабатывает; (d) video_only-батч → AudioSync false; (e) re-entry: strict-путь через dub-меню корректно возобновляется с восстановлением strict.
- [ ] **Step 2:** FAIL. **Step 3:** реализация. **Step 4:** PASS. **Step 5: gate + commit** `feat(tui): audio_sync ask prompt on download batches (PR165b T8)`.

---

### Task 9: финальный gate, интеграционные и живые пробы

- [ ] **Step 1: полный gate:** `gofmt -l .` (пусто) → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` → `make build-matrix`. 0 ошибок/предупреждений.
- [ ] **Step 2: интеграционный тест на синтетической паре с ffmpeg (hermetic, тег НЕ live — ffmpeg есть в CI-окружении проекта? проверить; если нет — тег `live`):** сгенерировать фикcачи Task 6 БЕЗ сдвига базы (donor.m4a + vid.mp4), прогнать `Analyzer.Reference` + `Analyzer.Offset` на pair (donor, donor+`-ss`-подмена) → найденный offset совпадает с известным в допуске ±0.05 c. Команда генерации — из Task 6.
- [ ] **Step 3: live-пробы (тег `live`, по конвенции live_*_test.go):**
  - `TestLiveMeSyncRealPair` (`internal/tui/live_pr165b_test.go`): реальная пара (эпизод anilib видео + дубляж с известным ручным сдвигом владельца из его таблицы, напр. 17.042) → assert |found − 17.042| ≤ 0.125 (ITU-допуск детектируемости), WindowsAgree ≥ 2, confidence ≥ 1. Форма доказательства: вывести в тест-лог found/expected/prominence per window; прогон вручную владельцем: `go test -tags live -run TestLiveMeSyncRealPair -count=1 -v ./internal/tui/`.
  - `TestLiveMeSyncMuxEndToEnd`: mux через `downloadOne`-подобную сборку с sync → `ffprobe` выходного файла: у аудио-дорожки (copy, offset>0) `start_time` ≈ 0; длительность аудио ≈ видео − offset.
- [ ] **Step 4:** результаты live-прогонов (если выполнялись) в `.sdd/pr165b-me-sync-verify.md`; коммит `test(tui): live me-sync pair proof (PR165b T9)`.

---

## 2. Сводный тест-план

| Слой | Что | Где | Метод |
|---|---|---|---|
| unit, чистая математика | точный лаг 12345/98765; несвязанные → prominence < 20; nextPow2 | correlate_test.go | синтетика, без exec |
| unit, вердикт | знак (+17.042/−0.958/6.25/0.5 найдены в ±0.05); тишина → ErrNoConfidence; диалог-only окно не голосует (agree=2); медиана 3 окон; snap сетка ±20 мс; расписание окон/MinEpisodeDur | analyze_test.go | синтетика |
| unit, exec-слой | argv декода (headers-блоб, -ss/-t до -i); s16le→float64; парс duration/fps; бюджет убивает декод; тишина → ErrNoReference | pcm/probe/offset_test.go | fake execFunc |
| unit, конфиг | parse/нормализация/дефолт audio_sync | config_test.go | toml-фикстуры |
| unit, применение | argv-golden: copy ±; фильтр v2 ±; mixed; \|off\|<0.001 → без флагов; legacy ± ; isHLS-таблица | mux_test.go | golden argv |
| unit, интеграция downloadOne | gate по task.AudioSync/provider; референс-цепочка; ретрай copy→filter (1 раз); SyncNote в отчёте; индекс не сломан | pr165b_me_sync_test.go | fake analyzer + fake mediaDownloader |
| unit, TUI ask | матрица config×сессия×режим; re-entry strict | pr165b_sync_ask_test.go | screen-тесты |
| integration | синтетическая пара через ffmpeg: Offset находит известный сдвиг | audsync (или live по доступности ffmpeg) | фикcачи Task 6 |
| live | реальная пара владельца: 17.042 ± 0.125; e2e mux + ffprobe-ассерты; HLS-пробы A/B/C | live_pr165b_test.go, `.sdd/pr165b-me-sync-verify.md` | `-tags live`, ручной прогон |

## 3. Оценки

| Область | Новые/изменённые LOC (с тестами) | Файлы |
|---|---|---|
| internal/audsync | ~500 кода + ~450 тестов | audsync.go, correlate.go, windows.go, analyze.go, exec.go, pcm.go, probe.go + 4 test-файла (новые) |
| internal/download | ~140 кода + ~220 тестов | mux.go, downloader.go, mux_test.go |
| internal/tui | ~260 кода + ~320 тестов | real.go, services.go, download_flow.go, session.go, app.go, pr165b_*_test.go, droppingDownload-фейк |
| config + i18n + toml | ~30 кода + ~60 тестов + ~15 строк toml | config.go, config_test.go, settings.example.toml, locales/{ru,en}.toml |
| deps/notices | ~10 строк | go.mod, go.sum, THIRD-PARTY-NOTICES.md |
| **итого** | **~2000 LOC** | 12 изменённых + ~12 новых |

## 4. Риски

1. **Пороги не откалиброваны на живых релизах** (MinRMS=100, MinProminence=20, ±80 мс, ConfidenceCap=10 — стартовые): исследовательские измерения — на белом шуме и локальных файлах. Митигация: константы экспортированы, live-проба владельца сверяет с его ручной таблицей; апгрейд на spectral-flux (BobineAudio-путь) не меняет пайплайна.
2. **HLS copy-путь не доказан до Task 6**: дефолт — консервативный фильтр v2 (перекодирование aac 256k у HLS-дубляжей со сдвигом — принятая цена ADDENDUM).
3. **«Мусор» вместо ошибки при -ss на экзотике** (несикуемый прямой источник): ретрай срабатывает только на ffmpeg-ОШИБКЕ; тихо-кривой результат не детектируется в v1 (research §8 это честно фиксирует).
4. **Дрейф (разные fps релизов) вне скоупа v1** — константа честно применяется; будущее — регрессия по якорям.
5. **Сигнатурный ripple DownloadService.Download** (+1 возвращаемое): один прод-имплементор, один тестовый фейк — дёшево, но заденет pr165_download_modes_test.go.
6. **AAC-прайминг ±21–23 мс** «гуляет» старт трека: snap (±20 мс) глушит; не чинится copy-путём (research §5.5) — не считать регрессией.
7. **Budget 15 c на пару в background-очереди**: ×N эпизодов растягивает батч; приемлемо (менеджер и так последовательный, concurrency 2).
8. **golangci-lint на gonum-циклы**: `gosec` на exec — уже решено в репо паттерном `//nolint:gosec // bin/args are config-derived` (downloader.go:221); повторить с той же мотивацией.

## 5. OWNER QUESTIONS (открытые пункты; остальные решения приняты по research/ADDENDUM)

1. **`aresample=async=1:first_pts=0` в цепочке v2** — v2-эскиз владельца его не упоминает, но research §5.3 (тоже по валидации владельца) прямо велит «сохранять как есть». План оставляет его финальной стадией. Подтвердить, или вычистить из v2-цепочки?
2. **Авто-флип HLS→copy при чистом PROBE C**: исполнитель вправе перевернуть `useFilterPath` в том же PR (с доказательствами в `.sdd/pr165b-me-sync-verify.md`) — или флип только после вашего подтверждения? Рекомендация: флип в том же PR.
3. **Background-режим без видимых sync-строк**: очередь печатает «фон» до анализа; применённые сдвиги в v1 попадают только в diagnostics-лог. Достаточно ли этого, или нужен post-settle канал менеджера (событие с заметкой) — отдельная маленькая задача?
