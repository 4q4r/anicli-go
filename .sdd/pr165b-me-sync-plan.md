<!-- markdownlint-disable MD010 -->
<!-- MD010 (hard tabs) отключён локально: Go-скичи в fenced-блоках обязаны
     использовать табы (gofmt); пробелы сломали бы вставку кода исполнителем. -->

# PR165b — план реализации «кросс-корреляция M&E» (автосинхронизация дубляжей), ред. 2

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** автоматическая правка рассинхрона дубляжа относительно видео через FFT-кросс-корреляцию аудио (decode → correlate → offset → apply), где СПОСОБ ПРИМЕНЕНИЯ сдвига выбирается эмпирически (Stage 0 проб-матрица), а каждый применённый сдвиг самопроверяется (звук с первой секунды) с лестницей ретраев.

**Architecture:** чистый пакет `internal/audsync` (ffmpeg-pipe PCM 8 кГц моно s16le → gonum FFT кросс-корреляция в 3 окнах → консенсус ≥2/3 ±80 мс → медиана → snap на сетку кадров → `SyncOffset`) — БЕЗ ИЗМЕНЕНИЙ от ред. 1. Применение — ПЕРЕРАБОТАНО: никакого заранее выбранного рецепта; Stage 0 эмпирически доказывает для каждой ячейки (тип источника × знак сдвига), какой метод (copy / фильтр-варианты) даёт звук с начала файла; продукт применяет метод-первенец ячейки, самопроверяет вывод через silencedetect и при провале повторяет с запасным методом; двойной провал — типизированная ошибка, файл НЕ отдаётся.

**Tech Stack:** Go 1.27, `gonum.org/v1/gonum/dsp/fourier` (v0.17.0, чистый Go, CGO off), ffmpeg/ffprobe n9+ (системная зависимость проекта), существующие пакеты download/tui/config/i18n.

**Spec:** `.sdd/pr165b-me-sync-research.md` (детекция и измерения; ЗДЕСЬ не воспроизводится). **Ред. 2 — по ОТКАЗУ владельца:** его фильтр-рецепт v2 (asetpts → trim/adelay → asetpts, aac 256k) в ручном ремюке дал «первую половину аниме без звука» — рецепт более НЕ является дефолтом, а способ применения выводится только из пробы (Stage 0) и охраняется самопроверкой.

## Global Constraints

- **ИНВАРИАНТ (жёсткое правило ред. 2): выданный файл ОБЯЗАН иметь слышимый звук со своей первой секунды.** Точная форма: измеренная ведущая тишина вывода обязана совпадать с ожидаемой геометрией сдвига (обрезка → ≈0; добивка → ≈|offset|) с допуском 1 c. Выход с молчащей головой — провал реализации, никогда не результат поставки (см. Task 7).
- Окружение для КАЖДОЙ go-команды: `export TMPDIR=/home/whoami/.cache/go-tmp GOTMPDIR=/home/whoami/.cache/go-tmp && mkdir -p $TMPDIR`.
- Quality gate на каждый таск: `gofmt -l` (пусто) → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` — 0 ошибок, 0 предупреждений.
- Кросс-платформа: `make build-matrix` (CGO_ENABLED=0); gonum чистый Go — безопасно.
- Анализ **никогда не валит загрузку**: ошибка анализа → mux как сегодня (без сдвига) + лог. Применение сдвига — НАОБОРОТ, охраняется: провал самопроверки после ретрая = провал загрузки (молчащий файл не отдаётся).
- Бюджет анализа: ≤15 c на пару (видео, дубляж), константа `DefaultBudget`.
- Корреляция только для пар «видео-провайдер ≠ провайдер дубляжа»; встроенное аудио видео — не анализировать.
- Sign-семантика (подтверждается Stage 0 эмпирически, п. «геометрия знака»): `offset > 0` — у донора «лишнее начало» → обрезать ведущую часть (ожидаемая ведущая тишина ≈ 0); `offset < 0` — донор опережает/не хватает донорского звука в начале → добить `|offset|` тишины (ожидаемая ведущая тишина ≈ |offset|). Форматирование секунд — 3 знака (`17.042`).
- Способ применения выбирается ТОЛЬКО по таблице, доказанной Stage 0 (`syncRecipeFor`); ячейка без доказанного метода → сдвиг для таких треков НЕ применяется (строка отчёта + лог), mux как сегодня. Метод, проваливший пробу, исключается из таблицы.
- Оффлайн-индекс: контракт не меняется (audio-only и marked-set файлы мимо индекса — как в PR165; legacy-путь индексируется как сегодня).
- Новая зависимость gonum разрешена владельцем (research §3.1); после добавления — `make notices` и сверка `THIRD-PARTY-NOTICES.md` (gonum: BSD-3-Clause).
- i18n ключи в `locales/en.toml` (истина) и `locales/ru.toml` (дефолт).
- План самодостаточен: константы/argv/рецепты скопированы, исследование заново не воспроизводится.

---

## 1. Архитектура решения (обзор, ред. 2)

```text
TUI «Скачать серии» (startRangeDownload)
  ├─ config download.audio_sync: ask → промпт «Синхронизировать дубляжи?» (1 раз на батч,
  │   только video_audio и merged-сессия >1 провайдера); yes/no → без промпта
  └─ DownloadTask{... AudioSync bool}
        │
        ▼
realCore.downloadOne (единственный хук; общий для foreground и background)
  ├─ resolve video/audio (как сегодня)
  ├─ eligible = треки (или отдельное аудио legacy) с провайдером ≠ video-провайдера
  ├─ ref := analyzer.Reference(video)            // 3×60 c, 1 раз на эпизод
  │     └─ ErrNoReference → fallback: первый eligible-дубляж как референс (если их ≥2)
  ├─ для каждого eligible: off := analyzer.Offset(ref, dub)    // ≤15 c, детекция БЕЗ ИЗМЕНЕНИЙ
  ├─ recipe := syncRecipeFor(isHLS(url), off > 0)              // таблица из Stage 0 (Task 1)
  │     └─ none (метод не доказан) → флаги не ставятся, SyncNote{unreliable}
  ├─ tracks[i].Sync = {Offset, Recipe}; input → c.dl.Download  // mux попытка №1
  ├─ САМОПРОВЕРКА (Task 7): VerifyAudioStart(вывод, первый sync-трек)
  │     ├─ ok (|ведущая тишина − ожидаемая| ≤ 1 c) → готово
  │     ├─ fail → mux попытка №2 с ЗАПАСНЫМ методом ячейки (SyncFallback)
  │     │        → самопроверка снова → ok → готово
  │     └─ fail → os.Remove(вывод) + типизированная ErrSilentLead (диагностика обеих попыток)
  └─ отчёт: «синхронизация: DUB +17.042с» / «нет уверенности» / «не применена: нет метода»
```

### 1.1 Метод-таблица применения (заполняется Stage 0, Task 1)

Stage 0 прогоняет для каждой ячейки **(тип источника × знак сдвига)** все методы-кандидаты и фиксирует, какой даёт звук с начала файла:

| Метод | argv-форма (offset = X, мс = M) |
|---|---|
| `copy` | `X>0`: `-ss X` перед `-i` донора; `X<0`: `-itsoffset {\|X\|}` перед `-i`; всегда `-c:a copy` |
| `v2_async` / `v2_plain` (рецепт владельца ± aresample) | `[N:a:0]asetpts=PTS-STARTPTS,{atrim=start=X / adelay=M:all=1},asetpts=PTS-STARTPTS{,aresample=async=1:first_pts=0}[a]` + `-c:a aac -b:a 256k` |
| `plain_async` / `plain_plain` (БЕЗ asetpts-цепочки — подозреваемый ломатель) | `[N:a:0]{atrim=start=X / adelay=M:all=1}{,aresample=async=1:first_pts=0}[a]` + `-c:a aac -b:a 256k` |

Шаблон результата (заполняется фактическими результатами Task 1 и переносится в `syncRecipeFor`):

| Ячейка | Первенец (доказан) | Запасной (доказан) | Исключены (причина) |
|---|---|---|---|
| (mp4, X>0) | *(из Stage 0)* | *(из Stage 0)* | *(…)* |
| (mp4, X<0) | *(из Stage 0)* | *(из Stage 0)* | *(…)* |
| (hls, X>0) | *(из Stage 0)* | *(из Stage 0)* | *(…)* |
| (hls, X<0) | *(из Stage 0)* | *(из Stage 0)* | *(…)* |

Правила заполнения: метод **доказан** для ячейки, если в выводе (i) нет ведущей тишины длиннее 1 c сверх ожидаемой геометрии (обрезка → ≤1 c; добивка → \|ведущая−\|X\|\| ≤ 1 c), (ii) позиция тона совпала с расчётной ±0.3 c, (iii) `ffprobe start_time` аудио sane (\|x\| ≤ 0.05). Первенец — доказанный метод с наименьшей ценой (copy дешевле перекодирования). Нет доказанных методов у ячейки → ячейка = `none` (сдвиг не применяется; инвариант важнее синхронизации).

Продуктовая лестница (Task 7-8) при этом ВСЕГДА активна как страховка: первенец → самопроверка → запасной → самопроверка → типизированная ошибка. Так «гибрид» из ред. 1 становится не отдельным методом, а свойством лестницы.

### 1.2 Геометрия знака (Stage 0 подтверждает эмпирически)

Фикстура Stage 0 строит донора с ИЗВЕСТНОЙ геометрией: тон на [5..35] c при 40 c длины; эталон — тот же сигнал без сдвига. Донор «+K» = контент появляется на K c позже (лишнее начало K c). Проверяется: метод «обрезать K» на доноре «+K» даёт тон на [5−K..] и тишину-голову ровно 5−K; метод «добить K» на доноре «−K» даёт тишину-голову 5+K. Параллельно фиксируется, что коррелятор (Task 2, синтетический юнит-пин) обязан назвать эту же геометрию «+K» — расхождение направления = стоп применению до выяснения (Task 4, замкнутый тест «измерил → применил → перемерил ≈ 0»).

Константы алгоритма детекции (все из research, в `internal/audsync/audsync.go`, без изменений от ред. 1):

| Константа | Значение | Смысл (research §) |
|---|---|---|
| `SampleRate` | 8000 | моно s16le PCM (§3.4) |
| `ReferenceDur` | 60.0 c | референс-окно (§4) |
| `SearchDur` | 180.0 c | поисковое окно (±60 c запаса) |
| `WindowCount` | 3 | окна на пару (§4) |
| `MinProminence` | 20.0 | пик/mean&#124;corr&#124; (§2.3) |
| `ConsensusTolerance` | 0.080 c | согласие окон (§4) |
| `SnapTolerance` | 0.020 c | допуск привязки к кадру (§5.4) |
| `DefaultFPS` | 24.0 | при нечитаемом r_frame_rate (§5.4) |
| `MinEpisodeDur` | 300.0 c | короче — анализ не запускать |
| `MinRMS` | 100.0 (s16, ≈ −50 dBFS) | тишина/пустое окно → не голосует (§2.4; калибровочное) |
| `ConfidenceCap` | 10.0 | cap confidence (§4; калибровочное) |
| `DefaultBudget` | 15 s | бюджет пары (§4) |

Расписание окон при длительности донора D (§4): старты референса `t₁ = 90`, `t₂ = D/2`, `t₃ = 2D/3`; референс декодируется `[t, t+60]` (у видео), поиск — `[max(0, t−60), 180]` (у донора). Rebasing: `offset_samples = D_lag − len(ref)`, `offset_sec = offset_samples / 8000`, диапазон ±60 c.

---

### Task 1: STAGE 0 — эмпирическая проб-матрица применения (ffmpeg, без кода)

**Files:**

- Create (временные фиксачи в `$TMPDIR/audsync-probe/`, команды ниже — дословно)
- Create: `.sdd/pr165b-me-sync-verify.md` (сырой вывод проб + ЗАПОЛНЕННАЯ таблица §1.1)

Это **первый шаг всего плана**: до написания какого-либо кода применения. Продукт-код (Task 6) пишется только против результатов этой задачи.

**Фиксачи** (тон 1 кГц на [5..35] c из 40 c; эталон = тот же донор БЕЗ сдвига базы):

```bash
export TMPDIR=/home/whoami/.cache/go-tmp; P=$TMPDIR/audsync-probe; mkdir -p $P; cd $P
FF='ffmpeg -hide_banner -loglevel error -y'
# донор: 5s тишина + 30s тон + 5s тишина, stereo aac
$FF -f lavfi -i "sine=frequency=1000:duration=30" -f lavfi -i "anullsrc=r=44100:cl=stereo" \
  -filter_complex "[0:a]aformat=sample_rates=44100:channel_layouts=stereo[t];[1:a]atrim=0:5[s1];[1:a]atrim=0:5[s2];[s1][t][s2]concat=n=3:v=0:a=1[a]" \
  -map "[a]" -c:a aac -b:a 192k donor.m4a
# видео-подложка 40 c + эталонная «встроенная дорожка» (тот же тайминг тона, что у невычисленного донора)
$FF -f lavfi -i "testsrc=duration=40:size=320x240:rate=24" -c:v libx264 -pix_fmt yuv420p vid.mp4
# три вкуса источника донора:
$FF -i donor.m4a -c copy mp4_zero.mp4                                   # прогрессивный, база PTS 0
$FF -i donor.m4a -c copy -output_ts_offset 3600 mp4_shifted.mp4         # прогрессивный, база +3600
$FF -i donor.m4a -c copy -output_ts_offset 3600 -f hls -hls_time 4 -hls_list_size 0 hls_shifted.m3u8  # HLS с сессионной базой (кейс владельца)
```

**Матрица:** вкусы {mp4_zero, hls_shifted} × знаки {+3 (обрезка), −3 (добивка)} × методы {copy, v2_async, v2_owner (v2 без aresample — точный эскиз владельца), plain_async, plain_plain} = 20 mux-прогонов (каждый — секунды). Вкус `mp4_shifted` — опциональный добор, только если основные ячейки покажут аномалии.

Шаблон прогона (обрезка +3, copy): `-i vid.mp4 -ss 3 -i <донор> -map 0:v:0 -map 1:a:0 -c copy out.mkv`; (добивка −3, copy): `-i vid.mp4 -itsoffset 3 -i <донор> … -c copy`; фильтр-методы: `-i vid.mp4 -i <донор> -map 0:v:0 -filter_complex "<цепочка метода из §1.1, X=3 / M=3000>" -map "[a1]" -c:v copy -c:a aac -b:a 256k out.mkv`.

**Ассерты каждого прогона** (все три — в протокол):

```bash
ffprobe -v error -select_streams a:0 -show_entries stream=start_time -of csv=p=0 out.mkv   # |x| ≤ 0.05
ffmpeg -hide_banner -i out.mkv -map 0:a:0 -t 15 -af silencedetect=noise=-50dB:d=0.2 -f null - 2>&1 | grep silence_
# обрезка +3: первая тишина-голова обязана кончиться на ≈2.0 (5−3) ±0.3, т.е. звук ≤ 2.3 c
# добивка −3:  тишина-голова ≈ 8.0 (5+3) ±0.3, и НЕ минуты (кейс-маркер поломки владельца)
```

Оценка «доказан»: все три ассерта в допуске И тон звучит ровно 30 c (хвост сохранён).

- [ ] **Step 1:** выполнить 20 прогонов; сырой вывод + вердикты — в `.sdd/pr165b-me-sync-verify.md`; заполнить таблицу §1.1 (первенец/запасной/исключены с причиной для каждой ячейки).
- [ ] **Step 2:** **геометрия знака** (два прогона из п. 1.2): «обрезать K» на доноре-лидере и «добить K» на доноре-отстающем дают расчётные позиции тона. Если НЕТ — стоп: соглашение о знаке пересматривается до Task 6.
- [ ] **Step 3:** коммит `docs(probe): stage-0 sync application matrix — proven method table (PR165b T1)` (файл .sdd добавляется `git add -f`).

---

### Task 2: пакет `internal/audsync` — типы, константы, чистая корреляция (gonum)

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
    Seconds      float64 // сдвиг донора, c; >0 = обрезать начало донора, <0 = добить тишину
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

- [ ] **Step 1: зависимость.** `export TMPDIR=... ; go get gonum.org/v1/gonum@v0.17.0 && go mod tidy`. `make notices` → добавить gonum (BSD-3-Clause) в `THIRD-PARTY-NOTICES.md`.

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

- [ ] **Step 3:** `go test -race -count=1 ./internal/audsync/ -run TestCrossCorrelate -v` → FAIL (CrossCorrelate undefined).
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

- [ ] **Step 5:** тесты из Step 2 — PASS.
- [ ] **Step 6: gate + commit** `feat(audsync): FFT cross-correlation core on gonum (PR165b T2)`.

---

### Task 3: `internal/audsync` — окна, вердикт (консенсус/медиана/confidence), snap

**Files:**

- Create: `internal/audsync/windows.go`
- Create: `internal/audsync/analyze.go` (только чистая часть: `analyzePair` + `snapToFrame`)
- Test: `internal/audsync/analyze_test.go`

**Interfaces:**

- Consumes: `CrossCorrelate`, константы (Task 2), `SyncOffset`.
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
func TestAnalyzePairFindsShiftedDubExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, k := range []float64{17.042, -0.958, 6.25, 0.5} {
		// видео-таймлайн 24 мин: вобл-сигнал с t=90; дубль-таймлайн = видео, сдвинутое на k;
		// окна нарезаются по WindowSchedule(1440) — refWins из «видео», searchWins из «дубляжа».
		refWins, searchWins := cutWindows(wobbleVideo(24*60, rng), shiftedCopy(k), 1440.0)
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
	// Окна 1 и 3: чистый сдвиг k=+2.0. Окно 2 — «диалог-only»: несвязанный шум
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
- [ ] **Step 3: реализация** `windows.go` + `analyze.go` (только чистые функции; orchestration с exec — Task 4). RMS-гейт ДО корреляции; медиана для 2 окон — среднее; `confidence = min(prom agrees)/MinProminence`, cap `ConfidenceCap`.
- [ ] **Step 4:** тесты PASS. **Step 5: gate + commit** `feat(audsync): window schedule, consensus verdict, frame snap (PR165b T3)`.

---

### Task 4: `internal/audsync` — exec-слой: ffmpeg-pipe PCM, ffprobe-пробы, `Reference`/`Offset` + замкнутый тест знака

**Files:**

- Create: `internal/audsync/exec.go`
- Create: `internal/audsync/pcm.go`
- Create: `internal/audsync/probe.go`
- Modify: `internal/audsync/analyze.go` (orchestration)
- Test: `internal/audsync/pcm_test.go`, `internal/audsync/probe_test.go`, `internal/audsync/offset_test.go`, `internal/audsync/live_loop_test.go` (тег `live`)

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

// Offset — вердикт для одного донора против готового Reference: проба длительности донора
// (format=duration) → расписание окон → декод 3×поисковых окон у донора → analyzePair.
// Бюджет: context.WithTimeout(Budget) на весь вызов (убивает ffmpeg по истечении).
// Сигнатура (SyncOffset, error); ошибки: ErrNoConfidence/ErrNoReference/обёртки с причиной.
func (a *Analyzer) Offset(ctx context.Context, ref Reference, dub contracts.VideoSource) (SyncOffset, error)
```

argv декода (research §3.4, дословно; заголовки — единым `-headers`-блобом как `headerArgs` в download; ресемплинг в моно 8 кГц делает сам ffmpeg флагами `-ac 1 -ar 8000` — отдельного Go-ресемплера НЕТ и не нужен):

```text
ffmpeg -hide_banner -loglevel error -nostdin [-headers "<CRLF-joined>"] -ss {start} -t {dur} -i {URL} -vn -ac 1 -ar 8000 -f s16le pipe:
```

argv пробы длительности: `ffprobe -v error -show_entries format=duration -of json {URL}` (+headers).
argv пробы fps: `ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of json {URL}` (+headers); парс `24000/1001` → 23.976…; нечитаемо → `DefaultFPS` (НЕ ошибка); нечитаемая duration донора = провал пары (обёртка ErrNoConfidence), duration видео = обёртка ErrNoReference.

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
	// fake exec отдаёт 8 сэмплов little-endian s16 → []float64; отрицательные и ±32768.
}

func TestParseDurationAndFPS(t *testing.T) {
	// duration: {"format":{"duration":"1440.5"}} → 1440.5; мусор/пусто → error (пара не анализируется).
	// fps: "24000/1001" → 23.976…; "24/1" → 24; мусор/ошибка пробы → DefaultFPS (НЕ ошибка анализа).
}

func TestOffsetBudgetKillsDecode(t *testing.T) {
	// fake exec спит до ctx.Done() (не time.Sleep!) с Options.Budget=200ms → Offset
	// возвращает обёртку ErrNoConfidence за ≤ 1s.
}

func TestReferenceErrOnSilence(t *testing.T) {
	// fake exec отдаёт нули → ErrNoReference.
}
```

- [ ] **Step 2:** FAIL. **Step 3: реализация** `exec.go`, `pcm.go` (байты → `binary.LittleEndian` → int16 → float64/32768), `probe.go`, orchestration в `analyze.go` (порядок: `Offset` = timeout → probeDuration → WindowSchedule → цикл окон; `Reference` = fps-проба + 3 декода `[t, t+60]` от dur видео).
- [ ] **Step 4:** PASS.
- [ ] **Step 5: замкнутый тест знака (тег `live`, ffmpeg обязателен; закрывает требование «пик ≈ 0 после применения»).**

```go
//go:build live

// TestLiveMeasuredAppliedRemeasuredZero: фикcачи Task 1 (donor.m4a + vid.mp4 со
// встроенной дорожкой того же тайминга). Донор «+3» собирается ffmpeg'ом (concat
// 3 c тишины + donor). Далее: Analyzer.Offset(videoSrc, dubSrc) → ожидался ≈ +3;
// применение ПЕРВЕНЦЕМ ячейки (mp4, +) из таблицы Task 1 — argv из .sdd/pr165b-me-sync-verify.md;
// затем ПОвторный Offset(видео, ПРИМЕНЁННЫЙ вывод) → |seconds| ≤ 0.125 (ITU).
// Форма доказательства: лог found1/applied/found2 в тестовый вывод.
func TestLiveMeasuredAppliedRemeasuredZero(t *testing.T) { /* ... */ }
```

- [ ] **Step 6:** `go test -race -count=1 ./internal/audsync/` PASS; live-прогон вручную: `go test -tags live -run TestLiveMeasuredAppliedRemeasuredZero -count=1 -v ./internal/audsync/`.
- [ ] **Step 7: gate + commit** `feat(audsync): exec layer, Reference/Offset, closed-loop sign proof (PR165b T4)`.

---

### Task 5: конфиг `download.audio_sync` + i18n

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

i18n-ключи (новые, в ОБА файла; третий — состояние «метод не доказан», ред. 2):

```toml
"download.sync_ask_title" = "Синхронизировать дубляжи по видео? (анализ ~15 с на пару)"
"download.sync_choice_yes" = "✓ Да — анализировать и править"
"download.sync_choice_no" = "✗ Нет — как есть"
"download.row_sync" = "синхронизация: {notes}"
"download.sync_note_applied" = "{dub} {offset}с"
"download.sync_note_uncertain" = "{dub}: нет уверенности"
"download.sync_note_unreliable" = "{dub}: не применена (нет проверенного метода)"
"download.sync_verdict" = "анализ синхронизации…"
```

- [ ] **Step 1: failing test** (`config_test.go`): парс `audio_sync = "yes"` → "yes"; `audio_sync = "мусор"` → нормализация в "ask"; отсутствие ключа → дефолт "ask".
- [ ] **Step 2:** FAIL. **Step 3:** поле + дефолт + `normalizeAudioSync` в load-пути; ключи в оба toml; `settings.example.toml`:

```toml
[download]
# ...
# Автосинхронизация дубляжей (кросс-корреляция M&E): ask — спросить
# перед батчем (по умолчанию), yes — всегда, no — никогда.
# audio_sync = "ask"
```

- [ ] **Step 4:** PASS. **Step 5: gate + commit** `feat(config): download.audio_sync ask|yes|no (PR165b T5)`.

---

### Task 6: применение сдвига — таблица методов Stage 0, оба билдера, isHLS

**Files:**

- Modify: `internal/download/mux.go` (AudioTrack, билдеры, таблица рецептов)
- Modify: `internal/download/downloader.go` (CommandInput)
- Test: `internal/download/mux_test.go` (argv-golden)

**Interfaces:**

```go
// mux.go
type SyncMethod string

const (
	SyncCopy   SyncMethod = "copy"   // -ss/-itsoffset + -c:a copy
	SyncFilter SyncMethod = "filter" // -filter_complex + aac 256k
)

// syncRecipe — применитель для одной ячейки (тип источника × знак).
type syncRecipe struct {
	Method SyncMethod
	Chain  string // для filter: "v2_async" | "v2_owner" | "plain_async" | "plain_plain" (§1.1)
}

// syncRecipeFor — ТАБЛИЦА, перенесённая из результатов Stage 0 (Task 1,
// .sdd/pr165b-me-sync-verify.md): первенец ячейки; fallback=false → запасной метод.
// Ячейка «none» (нет доказанных методов) → ok=false → сдвиг для трека НЕ применяется.
func syncRecipeFor(isHLS bool, positive bool, fallback bool) (r syncRecipe, ok bool)

func isHLS(rawURL string) bool // срезать query/fragment, ToLower, HasSuffix ".m3u8"
// syncPerInputArgs: offset>0 → ["-ss", "17.042"]; offset<0 → ["-itsoffset", "0.958"]; |x|<0.001 → nil.
func syncPerInputArgs(off float64) []string
// filterSyncChain — цепочка по варианту chain (§1.1), inputIdx — индекс -i; ms = round(|off|*1000).
//   v2_*:    "[N:a:0]asetpts=PTS-STARTPTS,{trim|delay},asetpts=PTS-STARTPTS{,aresample=async=1:first_pts=0}[aN]"
//   plain_*: "[N:a:0]{trim|delay}{,aresample=async=1:first_pts=0}[aN]"
func filterSyncChain(inputIdx int, off float64, chain string) string
```

```go
// downloader.go
type AudioTrack struct {
	// ...существующие поля...
	// Sync — вердикт анализа (nil = без сдвига; |Seconds|<0.001 флагов не порождает).
	Sync *audsync.SyncOffset
	// SyncFallback — ретрай-лестница: вместо первенца ячейки применить
	// запасной метод (syncRecipeFor(..., fallback=true)). Ставит только ladder (Task 8).
	SyncFallback bool
}

type CommandInput struct {
	// ...существующие поля...
	// AudioSync / AudioSyncFallback — то же для ОТДЕЛЬНОГО аудио legacy-пути.
	AudioSync        *audsync.SyncOffset
	AudioSyncFallback bool
}
```

Изменения билдеров (на `buildMuxArgs`; video-only/audio-only не трогаются):

```go
// 1) ВХОДЫ: per-input sync строго перед СВОИМ -i (после headerArgs), ТОЛЬКО для copy-метода:
for i, tr := range tracks {
	args = append(args, headerArgs(tr.Source.Headers)...)
	if r, ok := trackRecipe(tr); ok && r.Method == SyncCopy {
		args = append(args, syncPerInputArgs(tr.Sync.Seconds)...)
	}
	args = append(args, "-i", tr.Source.URL)
}
// 2) ФИЛЬТР-ТРЕКИ: одна цепочка -filter_complex (все chains через ";"), мапы [aN]
//    вместо N:a:0, per-stream кодек: "-c:a:N","aac","-b:a:N","256k".
//    trackRecipe(tr) = syncRecipeFor(isHLS(tr.Source.URL), tr.Sync.Seconds > 0, tr.SyncFallback);
//    ok=false → трек идёт БЕЗ флагов сдвига (ячейка none).
// 3) Порядок -map сохраняется — disposition/metadata циклы без изменений.
```

Legacy-ветка `BuildCommand` (default): `in.Audio != nil && in.AudioSync != nil` — те же два режима через `syncRecipeFor(isHLS(...), sign, fallback)`; фильтр: `-filter_complex "[1:a:0]{chain}[a1]" -map 0:v -map [a1] -c:v copy -c:a aac -b:a 256k`.

- [ ] **Step 1: failing argv-golden тесты.**

```go
func TestMuxCopyPathPositiveOffset(t *testing.T) { /* ... "-ss","17.042" перед -i прямого донора ... */ }
func TestMuxCopyPathNegativeOffset(t *testing.T) { /* "-itsoffset","0.958" перед -i */ }
func TestMuxFilterV2Positive(t *testing.T) {
	// HLS-донор: "-filter_complex","[1:a:0]asetpts=PTS-STARTPTS,atrim=start=17.042,asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0[a1]",
	// "-map","[a1]","-c:a:0","aac","-b:a:0","256k", НИКАКИХ -ss/-itsoffset.
	// (Variant v2_async — только если Stage 0 сделал его первенцем/запасным ячейки; иначе соответствующий вариант.)
}
func TestMuxFilterPlainNegative(t *testing.T) { /* adelay=958:all=1 по варианту ячейки */ }
func TestMuxMixedCopyAndFilter(t *testing.T) { /* трек 0 copy+(-ss), трек 1 фильтр [a2]; один -filter_complex */ }
func TestMuxNoneCellDropsSync(t *testing.T) { /* ячейка none → argv идентичен беcсдвиговому, лог-условие */ }
func TestMuxFallbackSwitchesMethod(t *testing.T) { /* SyncFallback=true → запасной метод ячейки */ }
func TestMuxZeroOffsetNoFlags(t *testing.T) { /* Sync{Seconds: 0.0004} → argv идентичен сегодняшнему */ }
func TestLegacySeparateAudioSync(t *testing.T) { /* AudioSync: copy и фильтр-режимы */ }
func TestIsHLSTable(t *testing.T) {
	// "http://x/a.m3u8"→true; ".../index.m3u8?tok=1#frag"→true; ".../A.M3U8"→true;
	// "http://x/a.mp4"→false; "http://x/m3u8.foo"→false.
}
```

ВАЖНО для исполнителя: конкретные варианты фильтр-цепочек в golden-тестах берутся ИЗ заполненной таблицы Task 1 (какая ячейка каким методом первенствует) — тесты кодируют РЕЗУЛЬТАТ пробы, а не предположение.

- [ ] **Step 2:** FAIL. **Step 3:** реализация по эскизу + таблица из Task 1. **Step 4:** PASS (весь `go test -race ./internal/download/`).
- [ ] **Step 5: gate + commit** `feat(download): stage-0 proven sync recipes — copy/filter builders, isHLS (PR165b T6)`.

---

### Task 7: самопроверка вывода — `VerifyAudioStart` + типизированная ошибка

**Files:**

- Modify: `internal/download/downloader.go` (метод верификатора)
- Test: `internal/download/verify_test.go` (hermetic, парсер), `internal/download/live_verify_test.go` (тег `live`, реальный ffmpeg)

**Interfaces:**

```go
// downloader.go
const (
	// LeadSilenceWindow — глубина самопроверки (первые N секунд вывода).
	LeadSilenceWindow = 15 * time.Second
	// MaxLeadDeviation — допуск «ведущая тишина vs ожидаемая геометрия» (инвариант ред. 2).
	MaxLeadDeviation = 1 * time.Second
)

// VerifyAudioStart измеряет ведущую тишину аудио-дорожки trackIdx файла path и
// сверяет с ожидаемой геометрией expectedLead: |measured − expected| ≤ MaxLeadDeviation.
// expectedLead: обрезка (offset>0) → 0; добивка (offset<0) → |offset|.
// argv: ffmpeg -hide_banner -i {path} -map 0:a:{trackIdx} -t 15 -af silencedetect=noise=-50dB:d=0.2 -f null -
// (вывод в stderr — читается существующим швом d.run). Парс: первый silence_start ≤ 0.05 →
// ведущая тишина = его silence_end; тишины в начале нет → 0.
func (d *Downloader) VerifyAudioStart(ctx context.Context, path string, trackIdx int, expectedLead time.Duration) error

// ErrSilentLead — двойной провал самоправерки: молчащий файл НЕ отдаётся
// (вывод удалён вызывающим lado лестницы). Диагностика обеих попыток — в Attempt.
type ErrSilentLead struct {
	Path     string
	Attempts []string // "copy: lead=8m12s (expected 0s)" и т.п.
}
func (e *ErrSilentLead) Error() string
```

- [ ] **Step 1: failing hermetic тесты (парсер + допуск).**

```go
func TestParseLeadSilence(t *testing.T) {
	// stderr c "silence_start: 0.0 … silence_end: 8.123" → 8.123s;
	// "silence_start: 4.2 …" (тишина НЕ с нуля) → 0; пустой stderr → 0; мусор → 0.
}
func TestVerifyAudioStartTolerance(t *testing.T) {
	// expected 0s, measured 8.123s → ошибка; measured 0.4s → nil;
	// expected 17s (добивка), measured 17.5s → nil; measured 3s → ошибка.
}
```

- [ ] **Step 2:** FAIL. **Step 3:** реализация парсера + метода (ffmpeg-зависимый прогон — в live-тесте).
- [ ] **Step 4: live-тест (тег `live`):** собрать фиксачу «3s тишина + тон» (mkv) → VerifyAudioStart(expected 0) → FAIL; фиксача «тон с 0 c» → nil; фиксача «добивка 2s» (тишина 2s + тон) → expected 2s → nil, expected 0 → FAIL.
- [ ] **Step 5: gate + commit** `feat(download): audio-lead self-verification, ErrSilentLead (PR165b T7)`.

---

### Task 8: интеграция в `downloadOne` — анализ, лестница ретраев, отчёт

**Files:**

- Modify: `internal/tui/real.go` (realCore: analyzer + verifier поля; `downloadOne`; `realDownload.Download`)
- Modify: `internal/tui/services.go` (DownloadService.Download сигнатура; DownloadTask.AudioSync)
- Modify: `internal/tui/download_flow.go` (runForegroundDownload: sync-заметки; рендер суффикса)
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
	Kind     string  // "applied" | "uncertain" | "unreliable" (ключи i18n sync_note_*)
}

// real.go — швы (по образцу dl/prober); nil = фича выключена.
type audioSyncAnalyzer interface {
	Reference(ctx context.Context, video contracts.VideoSource) (audsync.Reference, error)
	Offset(ctx context.Context, ref audsync.Reference, dub contracts.VideoSource) (audsync.SyncOffset, error)
}
type leadVerifier interface {
	VerifyAudioStart(ctx context.Context, path string, trackIdx int, expectedLead time.Duration) error
}
// продакшн: real.analyzer = audsync.New(audsync.Options{FFmpeg: "ffmpeg"}); real.verifier = real.dl
```

Порядок в `downloadOne` (после resolve video/tracks, до индексации):

1. Gate: `task.AudioSync && c.analyzer != nil`, иначе — как сегодня.
2. Сбор eligible-пар: (a) marked-set: треки с `providerOfTrackKey(key) != task.ProviderID`; (b) legacy: `input.Audio != nil` и провайдер отдельного аудио ≠ `task.ProviderID`.
3. Референс-цепочка (research §6.2): `Reference(video)`; `ErrNoReference` и eligible ≥ 2 → первый eligible-дубляж как референс; иначе skip с warn-логом.
4. Для каждого eligible: `Offset(...)`:
   - err → `SyncNote{Kind:"uncertain"}` + warn-лог;
   - уверенный ноль (|Seconds| < 0.001) → без заметки, info-лог «already in sync»;
   - иначе → рецепт ячейки `syncRecipeFor(...)`; `ok=false` → флаги НЕ ставятся, `SyncNote{Kind:"unreliable"}` + warn-лог; `ok=true` → `track.Sync = &off`, `SyncNote{Kind:"applied"}`, info-лог (offset, confidence, метод). Индекс первого sync-трека и его expectedLead (0 при >0, |off| при <0) запоминаются для верификации.
5. **Лестница (инвариант):** `c.dl.Download(ctx, input)` → если был хоть один sync-вход с методом → `c.verifier.VerifyAudioStart(ctx, outPath, trackIdx, expectedLead)`:
   - nil → готово;
   - err и у ячейки есть запасной метод → `input` c `SyncFallback=true` (все sync-входы) → ВТОРОЙ `c.dl.Download` → повторная `VerifyAudioStart`:
     - nil → готово (лог «recovered via fallback method»);
     - err → `os.Remove(outPath)` + вернуть `&download.ErrSilentLead{Path, Attempts:[первая, вторая]}` — молчащий файл НЕ отдаётся;
   - err и запасного метода нет → `os.Remove(outPath)` + `ErrSilentLead{Attempts:[первая]}`.
6. Вернуть `sync []SyncNote`; индексация — без изменений (формат EntryInput прежний).

Отчёт (foreground): `runForegroundDownload` принимает 4-е возвращаемое; `downloadEpisodeReport.Sync []SyncNote`; в `renderDownloadSettle` после dropped-суффикса:

```go
if sfx := renderSyncSuffix(r.Sync); sfx != "" {
	fmt.Fprintf(&b, " (%s)", sfx) // «синхронизация: DUB +17.042с, DUB2: нет уверенности»
}
// renderSyncSuffix: i18n.T("download.row_sync", {notes: strings.Join(notes, ", ")})
// applied:   fmt.Sprintf("%+.3f", sec) в "download.sync_note_applied"
// uncertain: "download.sync_note_uncertain"; unreliable: "download.sync_note_unreliable"
```

Background: заметки в лог (runDownload игнорирует возврат; downloadOne уже логирует всё); строки очереди — без суффикса (OWNER QUESTIONS #1).

- [ ] **Step 1: failing tests.**
  - fake analyzer (заданный SyncOffset / ошибка) + fake mediaDownloader (argv-капчер, может «писать» файлы) + fake leadVerifier.
  - Пин «молчащий вывод → ретрай»: fake verifier на 1-й вызов возвращает ошибку, на 2-й nil; assert: второй Download получил SyncFallback=true, файл отfaked как хороший, путь возвращён.
  - Пин «двойной провал»: verifier всегда ошибку → assert: второй вызов был, файл УДАЛЁН (fake dl пишет файл — тест проверяет отсутствие), ошибка `*download.ErrSilentLead` с 2 Attempts, downloadOne вернул её как err.
  - Пин «нет запасного → сразу ErrSilentLead с 1 Attempts».
  - Пин «верификация только при применённом сдвиге»: без sync verifier НЕ вызывается.
  - Пин «ячейка none»: трек без флагов, SyncNote unreliable, verifier не вызван.
  - Строка отчёта: «синхронизация: DUB +17.042с»; «нет уверенности»; «не применена (нет проверенного метода)».
  - offline-index: legacy-путь с sync создаёт запись индекса как сегодня.
- [ ] **Step 2:** FAIL. **Step 3:** реализация п.1–6 + правка фейка `droppingDownload` (pr165_download_modes_test.go:415) под новую сигнатуру. **Step 4:** PASS весь `go test -race ./internal/tui/`.
- [ ] **Step 5: gate + commit** `feat(tui): sync ladder in downloadOne — verify, fallback retry, ErrSilentLead (PR165b T8)`.

---

### Task 9: TUI-промпт `ask` + проводка на батч

**Files:**

- Modify: `internal/tui/session.go` (startRangeDownload, sync-меню), `internal/tui/app.go` (Deps.DownloadSync)
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
			f := true; syncChoice = &f
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

Гейт одноразовый по построению: промпт открывается только при `syncChoice == nil`, а обработчик всегда отвечает явным значением — re-entry loop невозможен. Режимы `video_only`/`audio_only` и одиночная сессия (`len(s.parts) < 2`) гейт не проходят вовсе.

- [ ] **Step 1: failing tests:** (a) config ask + merged-сессия → задачи с AudioSync true после «Да», false после «Нет»; (b) config yes/no → меню не открывается; (c) одиночная сессия → гейт не срабатывает; (d) video_only-батч → AudioSync false; (e) strict-путь через dub-меню корректно возобновляется (strict восстановлен из stash).
- [ ] **Step 2:** FAIL. **Step 3:** реализация. **Step 4:** PASS. **Step 5: gate + commit** `feat(tui): audio_sync ask prompt on download batches (PR165b T9)`.

---

### Task 10: финальный gate и живые пробы

- [ ] **Step 1: полный gate:** `gofmt -l .` (пусто) → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` → `make build-matrix`. 0 ошибок/предупреждений.
- [ ] **Step 2: live-пробы (тег `live`, по конвенции live_*_test.go):**
  - `TestLiveMeSyncRealPair` (`internal/tui/live_pr165b_test.go`): реальная пара (эпизод видео + дубляж с ручным сдвигом владельца из его таблицы, напр. 17.042) → assert: found ≈ 17.042 (±0.125, ITU), WindowsAgree ≥ 2, confidence ≥ 1; **и отдельно**: если found РАЗОШЁЛСЯ с ручным значением — тест НЕ падает, но пишет обе величины в лог (см. OWNER QUESTIONS #2 и риски).
  - `TestLiveMeSyncMuxEndToEnd`: полный mux с sync через сборку `downloadOne`-подобную → `VerifyAudioStart` PASS + `ffprobe`: длительность аудио ≈ видео − offset (обрезка).
  - Прогон владельцем: `go test -tags live -run 'TestLiveMeSync' -count=1 -v ./internal/tui/`.
- [ ] **Step 3:** результаты live-прогонов — в `.sdd/pr165b-me-sync-verify.md`; коммит `test(tui): live me-sync proofs (PR165b T10)`.

---

## 2. Сводный тест-план

| Слой | Что | Где | Метод |
|---|---|---|---|
| stage 0 | 20-прогонная матрица методов × источников × знаков: звук-с-начала, позиция тона ±0.3, start_time; геометрия знака | `.sdd/pr165b-me-sync-verify.md` | ffmpeg-прогоны по командам Task 1 |
| unit, математика | точный лаг 12345/98765; несвязанные → prominence < 20; nextPow2 | correlate_test.go | синтетика |
| unit, вердикт | знак (+17.042/−0.958/6.25/0.5 в ±0.05); тишина → ErrNoConfidence; диалог-only не голосует; медиана; snap; расписание | analyze_test.go | синтетика |
| unit, exec | argv декода; s16le→float64; duration/fps-парс; бюджет; ErrNoReference | pcm/probe/offset_test.go | fake execFunc |
| live, замкнутый цикл | измерил → применил первенцем ячейки → перемерил ≈ 0 (пик ~0 после применения) | live_loop_test.go | ffmpeg + Analyzer |
| unit, конфиг | parse/нормализация/дефолт audio_sync | config_test.go | toml |
| unit, применение | argv-golden: copy ±; фильтр по доказанным вариантам; mixed; none-ячейка; fallback-переключение; \|off\|<0.001 → без флагов; legacy; isHLS | mux_test.go | golden argv |
| unit, самопроверка | парсер lead; допуск \|measured−expected\| ≤ 1 c | verify_test.go | hermetic |
| live, самопроверка | реальные mkv: тишина-голова FAIL / тон с 0 nil / добивка expected-зависимо | live_verify_test.go | ffmpeg-фиксачи |
| unit, лестница | silent → ретрай fallback; двойной провал → удаление файла + ErrSilentLead(2); нет запасного → 1 Attempt; без sync — verify не зовётся; none-ячейка | pr165b_me_sync_test.go | fakes |
| unit, TUI ask | матрица config×сессия×режим; strict-restore | pr165b_sync_ask_test.go | screen-тесты |
| live | реальная пара: found vs ручная таблица владельца (расхождение НЕ падение, но в лог); e2e mux + verify + ffprobe | live_pr165b_test.go | `-tags live` |

## 3. Оценки (ред. 2)

| Область | Новые/изменённые LOC (с тестами) | Файлы |
|---|---|---|
| Stage 0 проба | ~0 кода (протокол ~200 строк md) | `.sdd/pr165b-me-sync-verify.md` |
| internal/audsync | ~520 кода + ~480 тестов | audsync.go, correlate.go, windows.go, analyze.go, exec.go, pcm.go, probe.go + 5 test-файлов (новые) |
| internal/download | ~230 кода + ~340 тестов | mux.go, downloader.go, mux_test.go, verify_test.go, live_verify_test.go |
| internal/tui | ~270 кода + ~340 тестов | real.go, services.go, download_flow.go, session.go, app.go, pr165b_*_test.go, droppingDownload-фейк |
| config + i18n + toml | ~30 кода + ~60 тестов + ~16 строк toml | config.go, config_test.go, settings.example.toml, locales/{ru,en}.toml |
| deps/notices | ~10 строк | go.mod, go.sum, THIRD-PARTY-NOTICES.md |
| **итого** | **~2250 LOC** | 12 изменённых + ~14 новых |

## 4. Риски и честные следствия отказа владельца

1. **Ручная таблица сдвигов владельца может не совпасть с измерениями коррелятора.** Его значения получены eyeballing'ем на конкретных релизах и применялись СЛОМАННЫМ методом — нельзя исключать, что часть «сдвигов» была скомпенсирована обратным знаком или вообще неверно измерена. Следствие: v1 считает источник истины пару «коррелятор на фактических источниках в момент скачивания»; ручная таблица — только земля для live-сверки (Task 10: расхождение пишется в лог, не падение). Согласование — OWNER QUESTIONS #2.
2. **Направление применения могло быть частью поломки владельца** (обрезал не тот конец). Защита: геометрия знака подтверждается дважды — фиксачами Stage 0 (Task 1, Step 2) и замкнутым «измерил → применил → перемерил ≈ 0» (Task 4, Step 5). До обоих — код применения не пишется.
3. **Ни один метод может не доказаться для какой-то ячейки** — тогда эта ячейка честно не синхронизируется (`none`), инвариант не нарушается, пользователь видит «не применена».
4. **Пороги детекции не откалиброваны на живых релизах** (MinRMS=100, MinProminence=20, ±80 мс) — стартовые; константы экспортированы; live-проба сверяет.
5. **Мульти-трековая верификация**: самопроверка покрывает ПЕРВЫЙ sync-трек вывода; катастрофа на втором-третьем треке не ловится в v1 (документировано; лог per-track остаётся).
6. **Дрейф (разные fps релизов) вне скоупа v1** — константа применяется честно; будущее — регрессия по якорям.
7. **Сигнатурный ripple DownloadService.Download** (+1 возвращаемое): один прод-имплементор + один тестовый фейк (`droppingDownload`).
8. **AAC-прайминг ±21–23 мс** гуляет старт трека: snap глушит; допуск верификации 1 c покрывает с запасом.
9. **Лестница удваивает цену провального mux** (2 прохода + 2 silencedetect): редкий путь (нормальный — 1 mux + 1 проверка ≤ 15 c декода).
10. **golangci-lint на exec-вызовы**: паттерн `//nolint:gosec // bin/args are config-derived` (downloader.go:221) повторить с той же мотивацией.

## 5. OWNER QUESTIONS

1. **Background-режим без видимых sync-строк:** очередь печатает «фон» до анализа; применённые сдвиги в v1 — только в diagnostics-лог (и сам файл). Достаточно, или нужен post-settle канал менеджера (событие с заметкой) — отдельная маленькая задача?
2. **Расхождение с ручной таблицей:** если live-проба покажет, что коррелятор меряет НЕ то, что ваша ручная таблица (после того как применение починено и проверено) — таблица перемеряется коррелятором и источник истины переходит к нему. Подтверждаете такой порядок?
