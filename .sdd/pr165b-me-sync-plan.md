<!-- markdownlint-disable MD010 -->
<!-- MD010 (hard tabs) отключён локально: Go-скичи в fenced-блоках обязаны
     использовать табы (gofmt); пробелы сломали бы вставку кода исполнителем. -->

# PR165b — план реализации «кросс-корреляция M&E», ред. 3 (mkvmerge-сборка)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** автоматическая правка рассинхрона дубляжа через FFT-кросс-корреляцию аудио, где сдвиг применяется БАЙТ-ТОЧНО к PCM (WAV), а финальный файл собирается `mkvmerge` — по архитектуре, предписанной владельцем после его root-cause расследования «первой половины без звука».

**Architecture:** детекция — пакет `internal/audsync` (ffmpeg-pipe PCM 8 кГц моно → gonum FFT кросс-корреляция 3 окон → консенсус ≥2/3 ±80 мс → медиана → snap на кадр → `SyncOffset`), БЕЗ ИЗМЕНЕНИЙ от ред. 2. Применение — НОВАЯ двухстадийная схема: (A) видео скачивается отдельно (`-map 0:v -c copy`), (B) каждый донор декодируется в WAV 48 кГц стерео → байтовый сдвиг PCM-полезной нагрузки на офсет → изолированный M4A (aac 256k), (C) сборка `mkvmerge --no-audio video + audio.m4a…` с языком/именем/диспозицией, (D) верификация RMS@45 c + геометрия головы. Одиночный ffmpeg-проход mux — только для загрузок БЕЗ синхронизации (его дефекты: fMP4-discontinuity + interleave flush — неприсущие mkvmerge-сборке).

**Tech Stack:** Go 1.27, `gonum.org/v1/gonum/dsp/fourier` v0.17.0 (чистый Go), ffmpeg/ffprobe (декод/кодирование аудио), **mkvmerge (mkvtoolnix) — новый startup-депенденси** (sysdeps, та же доктрина, что mpv/ffmpeg), существующие пакеты download/tui/config/i18n/sysdeps.

**Spec:** детекция — `.sdd/pr165b-me-sync-research.md` (не воспроизводится здесь). Применение — **root-cause владельца (ред. 3, переносим дословно, чтобы исполнитель не искал контекст):**

1. **fMP4-discontinuity:** веб-доноры (HLS) несут `#EXT-X-DISCONTINUITY` на границе Part A/Part B (10:26 — eyecatch под рекламу); половины имеют РАЗНЫЕ базовые PTS, а фрагментированный mp4 (moof/traf) не имеет линейных stts/stco — наивные trim/delay/copy неверно понимают, где реально начинается аудио.
2. **Interleave flush matroskaenc:** при `-c:v copy` (быстрые 1080p HEVC-пакеты) + запаздывающем AAC `av_interleaved_write_frame()` превысил `max_interleave_delta` (10 c) и вытолкнул монолитный блок 18611 видео-кадров (307 MB ≈ 13 мин) в MKV с НУЛЁМ аудио-пакетов → mpv: `audio/1: 0 packets` → тишина до 10-й минуты (лог владельца: `[mkv] Too many packets in the demuxer packet queues: video/0: 18611 packets, 307213408 bytes, audio/1: 0 packets`).
3. **Валидированное решение владельца:** донор → декод в PCM WAV (**форс `-ar 48000 -ac 2 -c:a pcm_s16le`**) → байтовый сдвиг payload по семантике `wave`-модуля (Go/Python; НЕ жёсткие 44 байта: ffmpeg вкладывает LIST/INFO — заголовок 78 байт; резать по маркеру чанка `data` или через wave-модуль) → изолированный M4A (`-c:a aac -b:a 256k`) → сборка `mkvmerge -o out.mkv --no-audio video.mkv --language 0:rus --track-name 0:"<dub>" --default-track-flag 0:yes audio.m4a` → верификация RMS@45 c + контрольный `mpv --no-resume-playback` с 00:00 (кэш watch-later не должен маскировать фикс).
4. **Офсеты владельца были ВЕРНЫ** (кратны кадру: +17.042 = вырезанные рекламные заставки Yummy/Wakanim на BD; −0.875 = тёмный преролл BD) — коррелятор меряет то же, что мерила его ручная таблица.

## Global Constraints

- **ИНВАРИАНТ (жёсткое правило): собранный файл ОБЯЗАН иметь слышимый звук в середине первой трети И корректную голову.** Проверка продукта (Task 8): RMS аудио на 45 c ≥ MinRMS (ловит именно кейс владельца «половина без звука») И ведущая тишина совпадает с ожидаемой геометрией сдвига (обрезка → ≈0, добивка → ≈|offset|) с допуском 1 c. Провал → файл удаляется, типизированная ошибка; молчащий/полумолчащий файл никогда не поставляется.
- Окружение для КАЖДОЙ go-команды: `export TMPDIR=/home/whoami/.cache/go-tmp GOTMPDIR=/home/whoami/.cache/go-tmp && mkdir -p $TMPDIR`.
- Quality gate на каждый таск: `gofmt -l` (пусто) → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` — 0 ошибок, 0 предупреждений; `make build-matrix` (CGO off).
- Анализ **никогда не валит загрузку**: ошибка анализа → mux как сегодня (без сдвига) + лог. Сборка/верификация — НАОБОРОТ: провал = провал эпизода (typed error).
- **mkvmerge — startup-депенденси** (sysdeps, `required` = mpv, ffmpeg, mkvmerge): отказ от установки НЕ брюшит старт; при отсутствующем mkvmerge синхронизация деградирует в обычную несинхронизированную загрузку с notice.
- Бюджет анализа ≤15 c на пару; анализ только для пар «провайдер видео ≠ провайдер донора»; встроенное аудио не анализируется.
- Sign-семантика сдвига (подтверждена владельцем и геометрией фикcач): `offset > 0` — обрезать `round(offset×48000)` кадров с начала WAV-payload; `offset < 0` — добить `round(|offset|×48000)` нулевых кадров. Кадр = 4 байта (2 канала × s16le). Формат секунд — 3 знака (`17.042`).
- **Пайплайн декода донора — 48 кГц стерео s16le (форс владельца); декод АНАЛИЗА — 8 кГц моно (research §3.4). Это два разных argv в audsync — не смешивать.**
- Одиночный ffmpeg-mux НЕ используется для синхронизированных эпизодов (root-cause 2 инtrinsic его single-pass форме); для всех несинхронизированных загрузок он остаётся ровно сегодняшним.
- Оффлайн-индекс: контракт не меняется (audio-only и marked-set мимо индекса; legacy индексируется как сегодня; sync не меняет форму EntryInput).
- Новая зависимость gonum разрешена (research §3.1); `make notices` + THIRD-PARTY-NOTICES.md (BSD-3-Clause). mkvmerge — внешняя программа, не go-зависимость.
- i18n ключи в `locales/en.toml` и `locales/ru.toml`.

---

## 1. Архитектура (ред. 3)

```text
TUI «Скачать серии» (startRangeDownload)
  ├─ config download.audio_sync: ask → промпт (1 раз на батч, video_audio + merged-сессия)
  └─ DownloadTask{... AudioSync bool}
        │
        ▼
realCore.downloadOne — развилка по «нужна ли сборка»:
  │
  ├─ НЕТ сборки (sync off / анализ не дал офсета ни одному донору / mkvmerge отсутствует):
  │    сегодняший одиночный ffmpeg-проход БЕЗ ИЗМЕНЕНИЙ + SyncNote{uncertain|unavailable}
  │
  └─ ДА сборка (≥1 донор с офсетом, mkvmerge на месте):
       Stage A  видео отдельно:  ffmpeg [-headers] -i videoURL -map 0:v:0 -c:v copy video.tmp.mkv
       Stage B  на КАЖДОГО донора (унифицированно; shift=0 если у трека нет уверенности):
         B1 декод:  ffmpeg [-headers] -i donorURL -map 0:a:0 -ar 48000 -ac 2 -c:a pcm_s16le -f wav donor.wav
         B2 сдвиг:  ShiftWAVFile(donor.wav, donor.shifted.wav, frames)   // байтовый, RIFF-safe
         B3 кодек:  ffmpeg -i donor.shifted.wav -c:a aac -b:a 256k donor.m4a
       Stage C  сборка:  mkvmerge -o out.part.mkv --no-audio video.tmp.mkv
                          [--chapters chapters.simple]
                          [--language 0:rus --track-name 0:"Dub" (--default-track-flag 0:yes|no)] dub.m4a …
                          --title "…"
       Stage D  верификация: VerifyAssembled(out) — lead-геометрия + RMS@45 c; провал → удалить + ErrSilentOutput
       финал:   rename out.part.mkv → out.mkv; удалить все промежуточные temp-файлы
```

### 1.1 Матрица деградации (per-episode)

| Ситуация | Поведение |
|---|---|
| `audio_sync=no` / промпт «Нет» / режимы video_only, audio_only | сегодняшний путь, анализ не запускается |
| Анализ: ни у одного донора нет офсета (no reference / no confidence) | сегодняшний путь (несинхронизированный mux) + строки «нет уверенности» |
| mkvmerge отсутствует (отказ на старте / пропал из PATH) | сегодняшний путь (несинхронизированный mux) + строка «синхронизация недоступна» + warn-лог |
| ≥1 донор с офсетом, сборка провалилась (exec/verify) | ЭПИЗОД ПРОВАЛЕН typed-ошибкой, temp-файлы удалены, молчащий файл не поставляется |
| Успех | `out.mkv` (sync-путь ВСЕГДА mkv) + строка «синхронизация: DUB +17.042с» |

Унификация Stage B: в собираемом эпизоде ВСЕ доноры проходят decode→shift→m4a (у трека без уверенности сдвиг = 0) — один mkvmerge-проход собирает все дорожки. Эпизод идёт по сборочному пути, если ХОТЯ БЫ один донор получил офсет.

### 1.2 Судьба старого ffmpeg-mux пути

**Остаётся без изменений** для: video_only, audio_only, legacy embedded/однопровайдерных пар, marked-set без полученных офсетов, всех деградаций из §1.1. **Никогда не используется** для синхронизированных эпизодов — root-cause 2 (interleave flush) intrinsic форме «один проход: `-c:v copy` + отстающее аудио». Синхронизационные argv-изменения из ред. 2 (`-ss`/`itsoffset` в buildMuxArgs, фильтр-цепочки, `syncRecipeFor`) **не реализуются вовсе** (они существовали только в плане). Никакого дублирующего fallback: одна сборочная схема.

### 1.3 fMP4-discontinuity и декод источника (исследование/проверка — директива владельца)

Ответ архитектуры (проверяется тестами Task 6 и Task 12):

- **Сырой PCM-декод НЕведом PTS**: `-f wav`/`s16le` отдаёт сэмплы по порядку декодирования; ffmpeg-HLS-демуксер скармливает пакеты через границу `#EXT-X-DISCONTINUITY` подряд, без пропусков сэмплов. Сдвиг по байтам PCM оперирует временем декодирования — **иммунен** к нелинейному таймлайну (в отличие от trim/delay ред. 2, работавших по PTS).
- **Сборка иммунна**: mkvmerge читает НАШИ промежуточные локальные файлы (mkv с непрерывным видео + m4a с честной линейной временной шкалой) — дисконтинуити в них не переносится.
- **Остаточная экспозиция — анализ**: входной `-ss t` (окна корреляции) ищет по нелинейному таймлайну; окно за границей 10:26 может декодировать не тот спан. Защита — консенсус (≥2 из 3 годных окон; «плохое» окно не проходит RMS/prominence-гейт и не голосует) + проба длительности приблизительно верна. Проверка: синтетический discontinuous-HLS фиксач (Task 6) и живой донор (Task 12).
- **speed-оценка сборки** (директива): mkvmerge ремюксирует 24-мин 1080p HEVC + 1-2 m4a без перекодирования видео — обычно 10-20× realtime (диск-упор); аудио-пайплайн добавляет декод+кодек ~5-10 c на донор. Итого +1-2 мин на эпизод против single-pass. Критерий владельца «realtime, если успевает» — выполняется с запасом; замер живым прогоном фиксируется в Task 12 (информационно, не гейт). TRUE-realtime mixing при просмотре — вне скоупа (директива 4: плеер не трогаем).

### 1.4 Константы детекции (research, `internal/audsync/audsync.go`; без изменений от ред. 2)

| Константа | Значение | Смысл |
|---|---|---|
| `SampleRate` | 8000 | декод АНАЛИЗА (моно s16le) |
| `PipelineRate` | 48000 | декод/сдвиг ПАЙПЛАЙНА (стерео s16le, форс владельца) |
| `ReferenceDur` / `SearchDur` | 60 / 180 c | окна (§4 research) |
| `WindowCount` | 3 | окна на пару |
| `MinProminence` | 20.0 | пик/mean&#124;corr&#124; (§2.3) |
| `ConsensusTolerance` | 0.080 c | согласие окон |
| `SnapTolerance` | 0.020 c | привязка к кадру |
| `DefaultFPS` | 24.0 | при нечитаемом r_frame_rate |
| `MinEpisodeDur` | 300.0 c | короче — анализ не запускать |
| `MinRMS` | 100.0 (s16) | тишина-гейт окон И верификация RMS@45 c |
| `ConfidenceCap` | 10.0 | cap confidence |
| `DefaultBudget` | 15 s | бюджет пары анализа |

Расписание окон: старты референса `t = 90, D/2, 2D/3`; референс `[t, t+60]` (видео), поиск `[max(0,t−60), 180]` (донор); `offset_samples = D_lag − len(ref)`; `offset_sec = offset_samples / 8000`; диапазон ±60 c.

---

### Task 1: sysdeps — mkvmerge третьим startup-депенденси

**Files:**

- Modify: `internal/sysdeps/sysdeps.go` (`required`), `internal/sysdeps/plan.go` (все планы), `internal/sysdeps/bootstrapFollowUp`
- Modify: `locales/en.toml`, `locales/ru.toml` (`deps.manual_*`)
- Test: `internal/sysdeps/sysdeps_test.go`, `internal/sysdeps/prompt_test.go`

**Interfaces / поведение (доктрина PR148 сохраняется целиком):**

```go
// sysdeps.go
// required — внешний порядок = порядок сообщения. mkvmerge (mkvtoolnix)
// замыкает медиа-тройку: воспроизведение, mux-декод, сборка.
var required = []string{"mpv", "ffmpeg", "mkvmerge"}
```

Обновление КАЖДОЙ команды установки (одна команда ставит всю тройку — форма доктрины «bundled install»):

| Платформа | Было | Стало |
|---|---|---|
| pacman | `sudo pacman -S mpv ffmpeg` | `sudo pacman -S mpv ffmpeg mkvtoolnix-cli` (extra repo — система владельца) |
| apt | `sudo apt install mpv ffmpeg` | `sudo apt install mpv ffmpeg mkvtoolnix` |
| dnf | `sudo dnf install --exclude=openh264* mpv ffmpeg` | `… mpv ffmpeg mkvtoolnix` |
| zypper | `sudo zypper install mpv ffmpeg` | `… mpv ffmpeg mkvtoolnix` |
| brew | `brew install mpv ffmpeg` | `brew install mpv ffmpeg mkvtoolnix` |
| winget | шаги `shinchiro.mpv`, `Gyan.FFmpeg` | + шаг `{argv: ["winget","install","-e","--id","MKVToolNix.MKVToolNix"]}` |
| choco | `choco install mpvio ffmpeg -y` | `choco install mpvio ffmpeg mkvtoolnix -y` |
| bootstrapFollowUp (brew/choco) | статические follow-up команды | + mkvtoolnix в те же команды |

i18n: `deps.manual_debian` → `… sudo apt install mpv ffmpeg mkvtoolnix`; `deps.manual_arch` → `… sudo pacman -S mpv ffmpeg mkvtoolnix-cli`; `deps.manual_fedora`/`deps.manual_suse` — аналогично + mkvtoolnix. Ключи `deps.missing_*`, `deps.offer_*` — generic, не меняются.

Фича-гейтинг: приложение, где пользователь ОТКАЗАЛСЯ от установки mkvmerge, продолжает работу (доктрина «never aborts»); синхронизация в `downloadOne` деградирует по §1.1 + стартовый notice уже называет mkvmerge (existing `MissingMessage`). Отдельного конфига пути mkvmerge НЕТ — LookPath при каждом запуске сборки (паритет с ffmpeg в `runFFmpeg`).

- [ ] **Step 1: failing tests.** Обновить ЗАКОНСЕРВЕННЫЕ argv-пины (sysdeps_test.go:173, 223, 281, 294, 319, 422, 465, 489): `sudo apt install mpv ffmpeg mkvtoolnix`, `brew install mpv ffmpeg mkvtoolnix`, winget-тройка, choco+mkvtoolnix; тест порядка `required` (mpv, ffmpeg, mkvmerge в сообщении); bootstrapFollowUp несёт mkvtoolnix; manual-ключи содержат mkvtoolnix/mkvtoolnix-cli.
- [ ] **Step 2:** FAIL. **Step 3:** правки `required`, планов, follow-up, локалей. **Step 4:** PASS (`go test -race ./internal/sysdeps/`).
- [ ] **Step 5: gate + commit** `feat(sysdeps): mkvmerge (mkvtoolnix) as third startup dependency (PR165b T1)`.

---

### Task 2: `internal/audsync` — типы, константы, чистая корреляция (gonum)

**Files:**

- Create: `internal/audsync/audsync.go`, `internal/audsync/correlate.go`
- Test: `internal/audsync/correlate_test.go`
- Modify: `go.mod`/`go.sum`, `THIRD-PARTY-NOTICES.md`

**Interfaces (produces):**

```go
package audsync

// SyncOffset — вердикт анализа одной пары (видео, донор). Нулевое значение =
// «нет уверенности» → без сдвига (research §6.3).
type SyncOffset struct {
	Seconds      float64 // >0 = обрезать начало донора; <0 = добить тишину
	Confidence   float64 // min(prominence)/MinProminence, cap ConfidenceCap; ≥1 у принятых
	WindowsAgree int     // согласных окон из годных (2 или 3)
}

var (
	ErrNoConfidence = errors.New("audsync: no confident offset")
	ErrNoReference  = errors.New("audsync: reference track is silent or absent")
)

const ( /* таблица §1.4 дословно, включая PipelineRate */ )
```

```go
// correlate.go — research §3.3:
// c = IFFT( FFT(x) · conj(FFT(y)) ), N = next_pow2(len(x)+len(y)−1); k = argmax(seq);
// D = (N−k) mod N — задержка srch относительно ref (srch[i] = ref[i−D]).
func nextPow2(n int) int
func CrossCorrelate(ref, srch []float64) (delay int, prominence float64, err error)
```

- [ ] **Step 1:** `go get gonum.org/v1/gonum@v0.17.0 && go mod tidy`; `make notices` → gonum в THIRD-PARTY-NOTICES.md.
- [ ] **Step 2: failing tests.**

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
			if prom < MinProminence { t.Fatalf("prominence = %v", prom) }
		})
	}
}

func TestCrossCorrelateUncorrelatedLowProminence(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	a := make([]float64, 48000); b := make([]float64, 144000)
	for i := range a { a[i] = rng.NormFloat64() }
	for i := range b { b[i] = rng.NormFloat64() }
	if _, prom, err := CrossCorrelate(a, b); err != nil || prom >= MinProminence {
		t.Fatalf("prom = %v err = %v, want prom < 20", prom, err)
	}
}

func TestNextPow2(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{1, 1}, {2, 2}, {3, 4}, {191999, 262144}, {262144, 262144}} {
		if got := nextPow2(tc.in); got != tc.want {
			t.Fatalf("nextPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 3:** запуск → FAIL (undefined). **Step 4: реализация** — `audsync.go` (doc: «аудио-автосинк дубляжей; детекция — research .sdd/pr165b-me-sync-research.md; применение — mkvmerge-архитектура владельца, ред. 3») + `correlate.go`:

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

- [ ] **Step 5:** PASS. **Step 6: gate + commit** `feat(audsync): FFT cross-correlation core on gonum (PR165b T2)`.

---

### Task 3: `internal/audsync` — окна, вердикт (консенсус/медиана), snap

**Files:**

- Create: `internal/audsync/windows.go`, `internal/audsync/analyze.go` (чистая часть)
- Test: `internal/audsync/analyze_test.go`

**Interfaces:**

```go
// windows.go
type Window struct { RefStart, SearchStart float64 } // секунды
// WindowSchedule: t = 90, D/2, 2D/3; nil если D < MinEpisodeDur.
func WindowSchedule(dur float64) []Window

// analyze.go — чистая функция (без exec): окна уже декодированы.
// Окно годно: RMS(ref) ≥ MinRMS && RMS(srch) ≥ MinRMS && prominence ≥ MinProminence;
// offset_i = (delay_i − len(ref_i))/SampleRate;
// ≥2 годных согласны в ±ConsensusTolerance → медиана → snapToFrame → SyncOffset; иначе ErrNoConfidence.
func analyzePair(refWins, searchWins [][]float64, fps float64) (SyncOffset, error)
func snapToFrame(off, fps float64) float64 // round(off*fps)/fps при |остатке| ≤ SnapTolerance, иначе off
func rms(x []float64) float64
func RMSOfPCM(pcm []byte) float64 // s16le → float64/32768 → rms; экспорт для верификации сборки (Task 8)
```

- [ ] **Step 1: failing tests.**

```go
func TestAnalyzePairFindsShiftedDubExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, k := range []float64{17.042, -0.958, 6.25, 0.5} {
		// видео-таймлайн 24 мин: вобл-сигнал (сумма синусов + шум, генератор из Task 2)
		// с t=90; дубль-таймлайн = видео, сдвинутое на k; окна по WindowSchedule(1440).
		refWins, searchWins := cutWindows(wobbleVideo(24*60, rng), shiftedCopy(k, rng), 1440.0)
		off, err := analyzePair(refWins, searchWins, 24)
		if err != nil { t.Fatalf("k=%v: %v", k, err) }
		if math.Abs(off.Seconds-k) > 0.05 { t.Fatalf("k=%v: got %v", k, off.Seconds) }
		if off.WindowsAgree < 2 || off.Confidence < 1 { t.Fatalf("k=%v: %+v", k, off) }
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
	// Окна 1,3 — сдвиг k=+2.0; окно 2 — несвязанный шум (другой seed) → не голосует.
	// Вердикт +2.0, WindowsAgree=2.
}

func TestAnalyzePairMedianOfThree(t *testing.T) {
	// Годные окна 2.000/2.004/2.080 (разброс ≤ 0.08) → медиана 2.004 ± 1e-9.
}

func TestSnapToFrame(t *testing.T) {
	cases := []struct{ name string; in, fps, want float64 }{
		{"snaps_within_tolerance", 17.050, 24, 17.041666},
		{"already_on_grid", 6.250, 24, 6.250},
		{"keeps_off_grid", 17.100, 24, 17.100},
		{"default_fps", 2.000, 0, 2.000}, // fps=0 → DefaultFPS
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fps := tc.fps
			if fps == 0 { fps = DefaultFPS }
			if got := snapToFrame(tc.in, fps); math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWindowSchedule(t *testing.T) {
	// D=1440: {90, 720, 960}; D=299 → nil; D=301 → 3 окна, start+60 ≤ D−1.
}

func TestRMSOfPCM(t *testing.T) {
	// 4 байта = 2 кадра s16le стерео: {1000,-1000,1000,-1000} → RMS=1000±1e-6; пусто → 0.
}
```

- [ ] **Step 2:** FAIL. **Step 3: реализация** (RMS-гейт до корреляции; медиана 2 окон — среднее; confidence = min(prom agrees)/MinProminence, cap). **Step 4:** PASS.
- [ ] **Step 5: gate + commit** `feat(audsync): windows, consensus verdict, frame snap, RMS helper (PR165b T3)`.

---

### Task 4: `internal/audsync` — exec-слой, `Reference`/`Offset`, argv пайплайна

**Files:**

- Create: `internal/audsync/exec.go`, `internal/audsync/pcm.go`, `internal/audsync/probe.go`
- Modify: `internal/audsync/analyze.go` (orchestration)
- Test: `internal/audsync/pcm_test.go`, `internal/audsync/probe_test.go`, `internal/audsync/offset_test.go`

**Interfaces:**

```go
// exec.go — шов (аналог download.runner); stdout буферизуется целиком.
type execFunc func(ctx context.Context, bin string, args []string) (stdout []byte, stderr string, err error)
func realExec(ctx context.Context, bin string, args []string) ([]byte, string, error)

// audsync.go
type Options struct {
	FFmpeg  string        // дефолт "ffmpeg"
	FFprobe string        // sibling от FFmpeg (169-K), иначе "ffprobe"
	Budget  time.Duration // DefaultBudget; НА ПАРУ анализа (Offset)
	Exec    execFunc      // nil = realExec
}
func New(opts Options) *Analyzer

// analyze.go
// Reference — 3 окна встроенной дорожки видео (1 раз на эпизод) + fps.
type Reference struct { Windows [][]float64; FPS float64 }
func (a *Analyzer) Reference(ctx context.Context, video contracts.VideoSource) (Reference, error)

// Offset — вердикт донора против Reference (≤ Budget; таймаут убивает ffmpeg).
func (a *Analyzer) Offset(ctx context.Context, ref Reference, dub contracts.VideoSource) (SyncOffset, error)
```

**Два ДЕКОДА — не путать:**

```go
// pcm.go — argv АНАЛИЗА (research §3.4, 8 кГц моно):
// ffmpeg -hide_banner -loglevel error -nostdin [-headers "<CRLF>"] -ss {start} -t {dur} -i {URL} -vn -ac 1 -ar 8000 -f s16le pipe:
func decodeArgs(src contracts.VideoSource, start, dur float64) []string

// pcm.go — argv ПАЙПЛАЙНА (форс владельца, 48 кГц стерео WAV, БЕЗ -ss: донор декодируется ЦЕЛИКОМ):
// ffmpeg -hide_banner -loglevel error -nostdin [-headers "<CRLF>"] -i {URL} -map 0:a:0 -ar 48000 -ac 2 -c:a pcm_s16le -f wav {out.wav}
func decodeWAVArgs(src contracts.VideoSource, outWAV string) []string

// pcm.go — argv кодека m4a:
// ffmpeg -y -hide_banner -loglevel error -i {in.wav} -c:a aac -b:a 256k {out.m4a}
func encodeM4AArgs(inWAV, outM4A string) []string

// probe.go — duration: ffprobe -v error -show_entries format=duration -of json {URL}
//            fps:      ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of json {URL}
// fps нечитаем → DefaultFPS (НЕ ошибка); duration донора нечитаема → обёртка ErrNoConfidence;
// duration видео нечитаема → обёртка ErrNoReference.
```

- [ ] **Step 1: failing tests (fake exec).**

```go
func TestDecodeArgs(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/v.m3u8", Headers: map[string]string{"Referer": "https://r/"}}
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-headers", "Referer: https://r/\r\n", "-ss", "90.000", "-t", "180.000",
		"-i", "https://x/v.m3u8", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:"}
	if got := decodeArgs(src, 90, 180); !reflect.DeepEqual(got, want) { t.Fatalf("got %q", got) }
}

func TestDecodeWAVArgsForcedPipeline(t *testing.T) {
	// пин форса владельца: "-map","0:a:0","-ar","48000","-ac","2","-c:a","pcm_s16le","-f","wav",out
	// и ОТСУТСТВИЕ -ss/-t.
}

func TestEncodeM4AArgs(t *testing.T) { // пин "-c:a","aac","-b:a","256k" }

func TestDecodePCMConvertsS16LE(t *testing.T) { // little-endian, отрицательные, ±32768 }

func TestParseDurationAndFPS(t *testing.T) {
	// duration "1440.5" → ok; мусор → error. fps "24000/1001" → 23.976…; мусор → DefaultFPS.
}

func TestOffsetBudgetKillsDecode(t *testing.T) {
	// fake exec спит до ctx.Done(); Budget=200ms → обёртка ErrNoConfidence за ≤1s.
}

func TestReferenceErrOnSilence(t *testing.T) { // нули → ErrNoReference }
```

- [ ] **Step 2:** FAIL. **Step 3: реализация** (`pcm.go`: байты→`binary.LittleEndian`→int16→float64/32768; orchestration: `Offset` = timeout → duration → schedule → 3 поиска; `Reference` = fps + 3 декода `[t, t+60]`). **Step 4:** PASS.
- [ ] **Step 5: gate + commit** `feat(audsync): exec layer, Reference/Offset, pipeline argv (PR165b T4)`.

---

### Task 5: `internal/audsync` — WAV RIFF-парсер и байтовый сдвиг

**Files:**

- Create: `internal/audsync/wav.go`, `internal/audsync/shift.go`
- Test: `internal/audsync/wav_test.go`, `internal/audsync/shift_test.go`, `internal/audsync/live_wav_test.go` (тег `live`)

**Interfaces:**

```go
// wav.go — семантика wave-модуля (Go-порт): RIFF-контейнер, чанки.
// ffmpeg кладёт перед data LIST/INFO-чанк (заголовок 78 байт) — жёсткие 44 байта ЗАПРЕЩЕНЫ.
type wavLayout struct {
	DataOff    int64 // абсолютное смещение payload чанка data
	DataSize   int64 // размер payload (из заголовка чанка)
	Frames     int64 // DataSize / blockAlign
	BlockAlign int16 // байт на кадр (каналы × 2)
}

// readWAVLayout читает RIFF-чанки по кругу (id, size LE, payload c pad-байтом при
// нечётном размере), валидирует RIFF/WAVE, fmt: PCM s16le, берёт ПЕРВЫЙ чанк data.
// Ошибки: не-RIFF, не-WAVE, не-PCM, нет fmt/data, битые размеры.
func readWAVLayout(f *os.File) (wavLayout, error)

// shift.go — файл-в-файл сдвиг БЕЗ загрузки в RAM (кадр-выровненные порции).
// frames > 0: обрезать frames кадров с начала payload (TrimPrefix-семантика);
// frames < 0: добить |frames| нулевых кадров. dst получает КОРЕКТНЫЕ заголовки
// (data size и RIFF size пересчитаны, pad-байт учтён), fmt-чанк копируется.
func ShiftWAVFile(src, dst string, frames int64) error

// удобство: FramesFor(seconds, rate) = int64(math.Round(seconds * rate))
func FramesFor(seconds float64, rate int) int64
```

- [ ] **Step 1: failing hermetic tests (рукописные RIFF-фикстуры).**

```go
func wavFixture(t *testing.T, withListChunk bool) string {
	// Руками собрать RIFF: RIFF/WAVE + fmt (PCM, 2ch, 48000, s16le, blockAlign=4)
	// + [опционально LIST/INFO 26 байт — кейс владельца «78-байтный заголовок»]
	// + data (N кадров) [+ pad-байт кейса нечётного INFO].
}

func TestReadWAVLayoutClassic44(t *testing.T) { /* DataOff=44, Frames=N */ }
func TestReadWAVLayoutWithListChunk(t *testing.T) { /* DataOff=78: LIST/INFO перед data НЕ ломает парсер */ }
func TestReadWAVLayoutRejects(t *testing.T) { /* не-RIFF; fmt=float; нет data; битый size → типизированные ошибки */ }

func TestShiftWAVFileTrim(t *testing.T) {
	// frames=+10: DataSize' = DataSize−40; payload начинается с кадра №10;
	// RIFF size пересчитан; первый байт dst.data == байт кадра 10 источника.
}
func TestShiftWAVFilePad(t *testing.T) {
	// frames=−10: DataSize' = DataSize+40; первые 40 байт payload — нули; дальше 1:1.
}
func TestShiftWAVFileZeroAndNegativeGuards(t *testing.T) { /* frames=0 → контент 1:1; |frames| > Frames при триме → типизированная ошибка (не молча пустой файл) */ }
func TestFramesFor(t *testing.T) { /* (17.042, 48000) = 818016; (−0.875, 48000) = −42000 */ }
```

- [ ] **Step 2:** FAIL. **Step 3: реализация.** **Step 4:** PASS hermetic.
- [ ] **Step 5: live-тест (тег `live`):** `ffmpeg -f lavfi -i sine -ar 48000 -ac 2 -c:a pcm_s16le -f wav` фиксач → readWAVLayout (реальный ffmpeg-заголовок с LIST/INFO: DataOff=78) → ShiftWAVFile ±1 c → повторный readWAVLayout валиден, размеры точны.
- [ ] **Step 6: gate + commit** `feat(audsync): RIFF-safe WAV parser and byte-exact PCM shift (PR165b T5)`.

---

### Task 6: замкнутый тест знака + fMP4-декод-проверка (live + синтетика)

**Files:**

- Test: `internal/audsync/live_loop_test.go` (тег `live`), `internal/audsync/discontinuity_test.go` (hermetic, синтетический HLS)

**Пин знака (закрывает «пик ≈ 0 после применения»):** фикcачи Task 12 (donor + video со встроенной дорожкой). Донор «+3 c» = concat(3 c тишины, donor). Полный цикл: `Offset(видео, донор+3)` ≈ +3 → `decodeWAVArgs` → `ShiftWAVFile(+round(3*48000))` → `encodeM4AArgs` → `Offset(видео, m4a-файл)` → |seconds| ≤ 0.125 (ITU). Форма доказательства: found1/shifted/found2 в тест-лог.

**fMP4-декод (директива «research/verify»):** синтетический discontinuous HLS собирается ffmpeg'ом: два mpegts-сегмента РАЗНОЙ геометрии + плейлист с `#EXT-X-DISCONTINUITY` между ними. Ассерты hermetic-теста (ffmpeg обязателен → тест живёт с тегом `live` рядом с hermetic-парсерами, но сам HLS-фиксач синтетический):

1. полный WAV-декод пайплайна (`decodeWAVArgs` без -ss) даёт длительность ≈ сумме сегментов ±1 c (декод сквозной, без дыр);
2. окно анализа `-ss 90 -t 60` декодирует ожидаемый спан (корреляция с известным контентом сегмента A);
3. окно, пересекающее границу дисконтинуити, деградирует честно: либо коррелирует, либо не голосует (RMS/prominence-гейт) — вердикт консенсуса от оставшихся окон не ломается.

- [ ] **Step 1:** фикcачи + тесты по спецификации выше. **Step 2:** прогон `go test -tags live -run 'TestLiveMeasuredShiftedRemeasured|TestLiveDiscontinuousHLSDecode' -count=1 -v ./internal/audsync/` — PASS; результаты в `.sdd/pr165b-me-sync-verify.md`.
- [ ] **Step 3:** hermetic-парсер плейлиста/фикcач (без ffmpeg) — PASS в основном suite.
- [ ] **Step 4: gate + commit** `test(audsync): closed-loop sign proof, discontinuous-HLS decode verification (PR165b T6)`.

---

### Task 7: `internal/download` — mkvmerge-сборщик, video-solo, конвертер глав

**Files:**

- Create: `internal/download/assemble.go`, `internal/download/chapters_mkv.go`
- Modify: `internal/download/downloader.go` (Options.MKVMerge, runner-переиспользование)
- Test: `internal/download/assemble_test.go`, `internal/download/chapters_mkv_test.go`, `internal/download/live_assemble_test.go` (тег `live`)

**Interfaces:**

```go
// downloader.go
type Options struct {
	FFmpeg  string // дефолт "ffmpeg"
	MKVMerge string // дефолт "mkvmerge"
}

// assemble.go
type AssembleAudio struct {
	Path    string // локальный .m4a донора (продукт Stage B)
	Lang    string // ISO 639-2 (LangToISO639_2 переиспользуется); "" → флаг опускается
	Title   string // имя дубляжа
	Default bool
}

type AssembleInput struct {
	VideoPath    string          // промежуточный video-only .mkv (Stage A)
	AudioFiles   []AssembleAudio // ≥1
	ChaptersFile string          // mkvmerge simple format ("") — флаг опускается
	OutputPath   string
	Title        string // --title (дефолт "Эпизод" — как в ffmpeg-билдерах)
}

// buildMkvMergeArgs — ДОСЛОВНАЯ форма владельца, обобщённая на N дорожек.
// Per-input опции строго ПЕРЕД своим входом; argv передаётся exec'ом напрямую
// (никакого шелл-цитирования: "0:<name>" — ОДИН argv-элемент).
func buildMkvMergeArgs(in AssembleInput) []string
// ["-o", out, "--no-audio", video,
//  "--language", "0:rus", "--track-name", "0:AniLibria.TV", "--default-track-flag", "0:yes", a1.m4a,
//  "--track-name", "0:DUB2", "--default-track-flag", "0:no", a2.m4a,
//  "--chapters", chapters.simple, "--title", title]

// RunAssemble исполняет mkvmerge. КОНТРАКТ КОДОВ ВЫХОДА mkvmerge:
// 0 = ok; 1 = ПРЕДУПРЕЖДЕНИЯ = успех (stderr в warn-лог); 2 = ошибка.
func (d *Downloader) RunAssemble(ctx context.Context, in AssembleInput) error
```

```go
// assemble.go — Stage A: промежуточный video-only mkv (без глав, без title — их ставит mkvmerge).
// ffmpeg -y -hide_banner -loglevel error [-headers blob] -i {videoURL} -map 0:v:0 -c:v copy {video.tmp.mkv}
func buildVideoSoloArgs(video contracts.VideoSource, outPath string) []string

// chapters_mkv.go — конвертер FFMETADATA → mkvmerge simple chapters.
// [CHAPTER] TIMEBASE=1/1000 START=35000 END=92000 title=intro
//   → "CHAPTER1=00:00:35.000\nCHAPTER1NAME=intro\n" (границы END игнорируются:
//     mkvmerge chapters — точки, не интервалы).
// Пустой/отсутствующий файл → "", флаг опускается. Временный файл рядом с выводом.
func ConvertChaptersToMkv(ffmetaPath, outDir string) (string, error)
```

Инвариант default-диспозиции переносится из `resolveAudioTracks`: ровно одна дорожка default; отсутствующая → первая (ремонт в вызывающем коде, Task 10 — здесь билдер честно рендерит как сказано).

- [ ] **Step 1: failing argv-пины.**

```go
func TestBuildMkvMergeArgsOwnerShape(t *testing.T) {
	in := AssembleInput{
		VideoPath: "v.tmp.mkv",
		AudioFiles: []AssembleAudio{{Path: "dub.m4a", Lang: "rus", Title: "AniLibria.TV", Default: true}},
		OutputPath: "out.part.mkv", Title: "Тайтл — эп 1",
	}
	want := []string{"-o", "out.part.mkv", "--no-audio", "v.tmp.mkv",
		"--language", "0:rus", "--track-name", "0:AniLibria.TV", "--default-track-flag", "0:yes", "dub.m4a",
		"--title", "Тайтл — эп 1"}
	if got := buildMkvMergeArgs(in); !reflect.DeepEqual(got, want) { t.Fatalf("got %q", got) }
}

func TestBuildMkvMergeArgsMultiTrack(t *testing.T) {
	// 2 дорожки: первая default=yes, вторая no; lang "" → флаг --language опущен для неё.
}
func TestBuildMkvMergeArgsChapters(t *testing.T) { /* "--chapters","c.simple" перед --title */ }
func TestBuildVideoSoloArgs(t *testing.T) { /* headers-блоб + "-map","0:v:0","-c:v","copy", out; без глав/title */ }
func TestRunAssembleExitCodes(t *testing.T) {
	// runner-шов: exit 0 → nil; exit 1 (stderr "warnings") → nil + warn;
	// exit 2 → типизированная ошибка с одной строкой stderr.
}
func TestConvertChaptersToMkv(t *testing.T) {
	// FFMETADATA-фикстура (2 главы, TIMEBASE 1/1000) → simple-формат, мс → HH:MM:SS.nnn;
	// пустой файл → ""; битая секция → типизированная ошибка.
}
```

- [ ] **Step 2:** FAIL. **Step 3: реализация.** **Step 4:** PASS.
- [ ] **Step 5: live (тег `live`):** собранные Task 12 фикcачи → полный RunAssemble → файл открывается ffprobe (v+2a-дорожки, language/default теги на месте); замер времени сборки 24-мин фикcачи → в `.sdd/pr165b-me-sync-verify.md` (speed-оценка §1.3).
- [ ] **Step 6: gate + commit** `feat(download): mkvmerge assembler, video-solo stage, FFMETADATA→mkv chapters (PR165b T7)`.

---

### Task 8: `internal/download` — VerifyAssembled (инвариант поставки)

**Files:**

- Create: `internal/download/verify.go`
- Test: `internal/download/verify_test.go` (hermetic), `internal/download/live_verify_test.go` (тег `live`)

**Interfaces:**

```go
// verify.go
const (
	LeadSilenceWindow = 15 * time.Second // глубина lead-проверки
	MaxLeadDeviation  = 1 * time.Second  // допуск геометрии головы
	RMSWindow         = 5 * time.Second  // окно RMS@45 c (пин владельца)
	RMSAt             = 45 * time.Second
)

// VerifyAssembled — ЕДИНАЯ проверка собранного файла (инвариант ред. 3):
//  1. RMS@45 c аудиодорожки 0:0 ≥ MinRMS — ловит «половина без звука» (root-cause 2).
//     argv: ffmpeg -hide_banner -i {path} -map 0:a:0 -ss 45 -t 5 -f s16le -ar 8000 -ac 1 pipe:
//     → audsync.RMSOfPCM.
//  2. Ведущая тишина совпадает с ожидаемой геометрией: |measured − expectedLead| ≤ MaxLeadDeviation.
//     argv: ffmpeg -hide_banner -i {path} -map 0:a:0 -t 15 -af silencedetect=noise=-50dB:d=0.2 -f null -
//     (stderr; парс: первый silence_start ≤ 0.05 → lead = его silence_end; тишины с нуля нет → 0).
//     expectedLead: обрезка (offset>0) → 0; добивка → |offset|.
// Любой провал → *ErrSilentOutput (файл удаляет вызывающий — Task 10).
func (d *Downloader) VerifyAssembled(ctx context.Context, path string, expectedLead time.Duration) error

type ErrSilentOutput struct {
	Path     string
	Check    string // "rms@45s" | "lead"
	Details  string // измеренное vs ожидаемое + одна строка stderr
}
func (e *ErrSilentOutput) Error() string
```

- [ ] **Step 1: failing hermetic тесты.**

```go
func TestParseLeadSilence(t *testing.T) {
	// "silence_start: 0.0 … silence_end: 8.123" → 8.123s; "silence_start: 4.2…" → 0; пусто/мусор → 0.
}
func TestVerifyAssembledTolerance(t *testing.T) {
	// fake runner: rms-вызов отдаёт звучащие байты, lead 0.4s, expected 0 → nil;
	// lead 8s expected 0 → ErrSilentOutput{Check:"lead"}; rms тишина → {Check:"rms@45s"};
	// expected 17s (добивка), lead 17.5s → nil.
}
```

- [ ] **Step 2:** FAIL. **Step 3: реализация.** **Step 4:** PASS.
- [ ] **Step 5: live:** фикcачи «3s тишина+тон» (FAIL по lead), «тон с 0» (nil), «добивка 2s» (nil при expected 2s) — `go test -tags live -run TestLiveVerifyAssembled -v ./internal/download/`.
- [ ] **Step 6: gate + commit** `feat(download): assembled-file verification — RMS@45s + lead geometry (PR165b T8)`.

---

### Task 9: конфиг `download.audio_sync` + i18n

**Files:**

- Modify: `internal/config/config.go` (~118 структура, ~411 дефолты), `settings.example.toml` (~152), `locales/en.toml`, `locales/ru.toml`
- Test: `internal/config/config_test.go`

```go
type Download struct {
	MaxConcurrency int `toml:"max_concurrency"`
	// AudioSync управляет автосинхронизацией дубляжей: "ask" — промпт на
	// батч (дефолт), "yes" — всегда, "no" — никогда. Неизвестное значение
	// деградирует в "ask" при загрузке (строка в лог, старт не брюшится).
	AudioSync string `toml:"audio_sync"`
}
```

i18n-ключи (в ОБА файла):

```toml
"download.sync_ask_title" = "Синхронизировать дубляжи по видео? (анализ ~15 с на пару)"
"download.sync_choice_yes" = "✓ Да — анализировать и править"
"download.sync_choice_no" = "✗ Нет — как есть"
"download.row_sync" = "синхронизация: {notes}"
"download.sync_note_applied" = "{dub} {offset}с"
"download.sync_note_uncertain" = "{dub}: нет уверенности"
"download.sync_note_unavailable" = "{dub}: синхронизация недоступна (mkvmerge не установлен)"
"download.sync_verdict" = "анализ синхронизации…"
```

`settings.example.toml` `[download]`: закомментированный `# audio_sync = "ask"` с комментарием (ask/yes/no; sync-файлы всегда .mkv через mkvmerge).

- [ ] **Step 1: failing test** (`audio_sync="yes"` → "yes"; `"мусор"` → "ask"; отсутствие → "ask"). **Step 2:** FAIL. **Step 3:** поле+дефолт+`normalizeAudioSync`; ключи; toml. **Step 4:** PASS.
- [ ] **Step 5: gate + commit** `feat(config): download.audio_sync ask|yes|no (PR165b T9)`.

---

### Task 10: tui — двухстадийный `downloadOne`, лестница деградации, отчёт

**Files:**

- Modify: `internal/tui/real.go` (realCore: analyzer, verifier-швы; `downloadOne`; `runDownload` — wire progress)
- Modify: `internal/tui/services.go` (DownloadService.Download сигнатура; DownloadTask.AudioSync)
- Modify: `internal/tui/download_flow.go` (SyncNote; renderDownloadSettle суффикс)
- Test: `internal/tui/pr165b_me_sync_test.go`

**Interfaces:**

```go
// services.go
type DownloadTask struct {
	// ...существующие поля...
	// AudioSync — решение «анализировать ли» (конфиг + промпт, Task 11).
	AudioSync bool
}

type DownloadService interface {
	Download(ctx context.Context, task DownloadTask) (path string, dropped []string, sync []SyncNote, err error)
	Submit(task DownloadTask)
	ActiveBanner() string
}

// download_flow.go
type SyncNote struct {
	Dub     string
	Seconds float64
	Kind    string // "applied" | "uncertain" | "unavailable"
}

// real.go — швы (nil = фича выключена; продакшн: audsync.New + real.dl):
type audioSyncAnalyzer interface {
	Reference(ctx context.Context, video contracts.VideoSource) (audsync.Reference, error)
	Offset(ctx context.Context, ref audsync.Reference, dub contracts.VideoSource) (audsync.SyncOffset, error)
}
type assembleVerifier interface {
	VerifyAssembled(ctx context.Context, path string, expectedLead time.Duration) error
}
```

Порядок в `downloadOne`:

1. Gate `task.AudioSync && c.analyzer != nil`; resolve video + треки как сегодня.
2. Референс-цепочка: `Reference(video)`; `ErrNoReference` и доноров ≥ 2 → первый донор-референс; иначе skip.
3. Офсеты: `Offset(ref, donor)` на каждого cross-provider донора; err → SyncNote{uncertain}; ноль → без заметки (info «already in sync»).
4. Развилка §1.1: ни одного офсета ИЛИ mkvmerge не найден (`exec.LookPath(c.dl.mkvmerge)` — шов `mkvmergeLooker` для тестов) → сегодняшний ffmpeg-путь + SyncNote{uncertain|unavailable}; иначе — сборочный путь:
   - Stage A: `buildVideoSoloArgs` → `c.dl.run` → `video.tmp.mkv`;
   - Stage B: на каждого донора: `decodeWAVArgs` → `ShiftWAVFile(wav, wav.sh, FramesFor(off.Seconds, PipelineRate))` → `encodeM4AArgs`; у no-confidence доноров frames=0;
   - главы: `ConvertChaptersToMkv(chaptersFile, outDir)` (ffmeta temp уже резолвится сегодняшним кодом);
   - Stage C: `RunAssemble(AssembleInput{…, OutputPath: TempPathFor(outPath)})`; default-ремонт как в `resolveAudioTracks`;
   - Stage D: `VerifyAssembled(out.part.mkv, expectedLead)` — expectedLead от ПЕРВОГО трека с офсетом (0 при обрезке, |off| при добивке); провал → `os.Remove(out.part.mkv)` + `*download.ErrSilentOutput`;
   - финал: `os.Rename` (существующий контракт TempPathFor/атомарности); удалить все `.tmp`-промежуточные (defer, best-effort — форму chapters-cleanup downloader.go:964);
   - имя/индекс: расширение `mkv`; правило индекса прежнее (legacy → UpsertEntry, marked-set → skip).
5. Manager progress: `runDownload` перестаёт игнорировать progress-колбэк — 10 (видео готово) / 30 (аудио готово) / 90 (сборка готова) / 100. Foreground-строки не меняются.
6. Вернуть `sync []SyncNote`; рендер суффикса строки — `renderSyncSuffix` (как ред. 2: «(синхронизация: DUB +17.042с, DUB2: нет уверенности)»); background — только логи (решение владельца Q1).

- [ ] **Step 1: failing tests (fakes: analyzer, mediaDownloader/runner-капчер, verifier, mkvmergeLooker).**
  - Пин «оффсет есть → сборочный путь»: последовательность argv — video solo, WAV-декод донора (без -ss), m4a-кодек, mkvmerge с `--no-audio`; расширение .mkv.
  - Пин «ни одного офсета → сегодняшний ffmpeg-путь» (argv идентичен текущему mux) + SyncNote{uncertain}.
  - Пин «mkvmerge отсутствует → ffmpeg-путь» + SyncNote{unavailable}.
  - Пин «verify fail → файл удалён + ErrSilentOutput наверх»; «verify ok → rename, temps удалены».
  - Пин «донор без уверенности в собираемом эпизоде → frames=0, всё равно m4a».
  - Пин индекса: legacy-синхронизированный файл индексируется как сегодня; marked-set — нет.
  - Строка отчёта: applied/uncertain/unavailable рендер.
- [ ] **Step 2:** FAIL. **Step 3:** реализация + фейк `droppingDownload` (pr165_download_modes_test.go:415) под новую сигнатуру. **Step 4:** PASS `go test -race ./internal/tui/`.
- [ ] **Step 5: gate + commit** `feat(tui): two-stage sync download — video solo, PCM shift, mkvmerge assembly (PR165b T10)`.

---

### Task 11: TUI-промпт `ask` + проводка на батч

**Files:**

- Modify: `internal/tui/session.go`, `internal/tui/app.go` (Deps.DownloadSync), `internal/tui/real.go` (wiring)
- Test: `internal/tui/pr165b_sync_ask_test.go`

**Interfaces / поведение (без изменений от ред. 2):**

```go
// app.go: type Deps struct { /* ... */ DownloadSync string } // "" ведёт себя как "ask"
// session.go: поля syncAskMode string, syncAskStrict bool; параметр syncChoice *bool:
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
		default: // ask: stash + да/нет-меню (форма dub-меню #164)
			s.syncAskMode, s.syncAskStrict = mode, strict
			s.openSyncAskMenu()
			return s, nil
		}
	}
	ask := *syncChoice
	// tasks: AudioSync: ask; dispatch foreground/background — как сегодня.
}
// handleSyncAskKey: «Да» → true; «Нет»/Esc → false (батч продолжается БЕЗ sync — не отмена);
// затем startRangeDownload(stash, stash, &choice). Существующие вызовы (session.go:2856, :2986) → nil.
```

- [ ] **Step 1: failing tests:** (a) ask+merged → «Да» AudioSync true / «Нет» false; (b) yes/no → меню нет; (c) одиночная сессия → гейт мимо; (d) video_only → false; (e) strict-restore через dub-меню.
- [ ] **Step 2:** FAIL. **Step 3:** реализация. **Step 4:** PASS.
- [ ] **Step 5: gate + commit** `feat(tui): audio_sync ask prompt on download batches (PR165b T11)`.

---

### Task 12: финальный gate и живые доказательства

- [ ] **Step 1: полный gate:** `gofmt -l .` → `go vet ./...` → `golangci-lint run` → `go test -race -count=1 ./...` → `make build-matrix`. 0/0.
- [ ] **Step 2: live (тег `live`, `internal/tui/live_pr165b_test.go`):**
  - `TestLiveMeSyncRealPair`: реальная пара (видео+дубляж с ручным сдвигом владельца +17.042): found ≈ 17.042 ± 0.125, WindowsAgree ≥ 2, confidence ≥ 1; полный пайплайн (solo → wav → shift → m4a → mkvmerge) → VerifyAssembled PASS; **контроль владельца**: `mpv --no-resume-playback out.mkv` с 00:00 — звук с первой секунды и на 45-й (ручной шаг, результат — в `.sdd/pr165b-me-sync-verify.md`).
  - `TestLiveMeSyncAssemblySpeed`: замер Stage A+B+C на 24-мин фикcаче → скорость vs realtime в verify.md (§1.3, информационно).
  - Прогон: `go test -tags live -run 'TestLiveMeSync' -count=1 -v ./internal/tui/ ./internal/audsync/ ./internal/download/`.
- [ ] **Step 3:** результаты в verify.md; коммит `test: live me-sync proofs — real pair, assembly speed, owner playback check (PR165b T12)`.

---

## 2. Сводный тест-план

| Слой | Что | Где | Метод |
|---|---|---|---|
| unit, sysdeps | required-тройка и порядок; argv планов с mkvtoolnix(-cli); follow-up; manual-ключи | sysdeps_test.go | обновлённые пины |
| unit, математика | точный лаг; несвязанные → prom < 20; nextPow2 | correlate_test.go | синтетика |
| unit, вердикт | знак (+17.042/−0.958/6.25/0.5 ±0.05); тишина → ErrNoConfidence; консенсус/медиана; snap; расписание; RMSOfPCM | analyze_test.go | синтетика |
| unit, exec | argv анализа (8k моно); argv пайплайна (48k стерео WAV, форс, без -ss); m4a-кодек; duration/fps; бюджет; ErrNoReference | pcm/probe/offset_test.go | fake execFunc |
| unit, WAV/сдвиг | RIFF-парсер: 44-байтный, 78-байтный LIST/INFO (кейс владельца), pad-байт, отказы; ShiftWAVFile ±кадры точные размеры/контент; FramesFor | wav/shift_test.go | рукописные RIFF |
| live, замкнутый знак | измерил +3 → сдвинул → m4a → перемерил ≈ 0 | live_loop_test.go | ffmpeg |
| live/hermetic, fMP4 | discontinuous HLS: сквозной декод без дыр; окна за границей деградируют честно | discontinuity_test.go, live | синтетический HLS |
| unit, сборка | argv пины владельца (одна/две дорожки, главы, title); video-solo; exit-коды 0/1/2; конвертер глав | assemble/chapters_mkv_test.go | golden argv |
| unit, верификация | lead-парсер; допуск геометрии; RMS-гейт; ErrSilentOutput | verify_test.go | hermetic |
| live, верификация | реальные mkv: тишина-голова FAIL; тон с 0 nil; добивка expected-зависимо | live_verify_test.go | ffmpeg-фиксачи |
| unit, downloadOne | развилка §1.1 (все ветки); последовательность стадий; verify-fail → удаление+ошибка; индекс; SyncNote-рендер | pr165b_me_sync_test.go | fakes |
| unit, TUI ask | матрица config×сессия×режим; strict-restore | pr165b_sync_ask_test.go | screen |
| live | реальная пара +17.042: полный пайплайн + VerifyAssembled + mpv-контроль владельца; speed-замер | live_pr165b_test.go | `-tags live` |

## 3. Оценки (ред. 3)

| Область | LOC (код+тесты) | Файлы |
|---|---|---|
| sysdeps | ~40 + ~120 | sysdeps.go, plan.go, 2 test-файла, локали deps.manual_* |
| audsync детекция | ~560 + ~500 | audsync.go, correlate.go, windows.go, analyze.go, exec.go, pcm.go, probe.go + тесты |
| audsync WAV/сдвиг | ~160 + ~240 | wav.go, shift.go + тесты |
| download сборка+верификация | ~280 + ~440 | assemble.go, chapters_mkv.go, verify.go + тесты |
| tui двухстадийный+ask | ~320 + ~420 | real.go, services.go, download_flow.go, session.go, app.go + тесты |
| config + i18n + toml | ~30 + ~60 + ~20 строк | config.go, settings.example.toml, locales |
| deps/notices | ~10 | go.mod, go.sum, THIRD-PARTY-NOTICES.md |
| **итого** | **~2740** | 14 изменённых + ~17 новых |

## 4. Риски

1. **Доступность mkvmerge**: новый внешний депенденси. Митигировано доктриной sysdeps (детект/промпт/non-TTY/never-abort) + деградацией §1.1; риск — пользователь в отказе получает несинхронизированные файлы без явного красного флага: строка отчёта «синхронизация недоступна» + стартовый notice.
2. **Разброс версий mkvmerge**: флаг `--default-track-flag` — современное имя (mkvtoolnix ≥ 54, 2021; в distro-репо давно). Решение (без probe-версии, YAGNI): пол `mkvtoolnix ≥ 54` документируется; незнакомый флаг = exit 2 = typed failure с stderr. (**Veto-able design call** — если нужна поддержка старых дистров, добавить `--version`-пробу в sysdeps.)
3. **Диск/RAM пайплайна**: промежуточный WAV донора ≈ 550 MB на 24 мин (48 кГц стерео s16le) + m4a ~120 MB; ShiftWAVFile файл-в-файл (RAM-flat). Пики ~0.7 GB на донора, очистка best-effort deferred. При жалобах на диск — будущая оптимизация: потоковый shift через pipe (ffmpeg→shift→m4a без wav на диске).
4. **fMP4-дисконтинуити в анализе**: входной `-ss` по нелинейному таймлайну может дать окно не с того места → консенсус ≥2/3 поглощает; закрыто тестами Task 6 и живым донором Task 12. В применении дефект не воспроизводим (PCM-декод + mkvmerge оба иммунны — §1.3).
5. **Скорость сборки**: оценка 10-20× realtime (ремюкс без перекодирования); критерий владельца «realtime, если успевает» выполняется; замер Task 12 фиксирует факт. Худший случай (медленный HDD) — «1× realtime» на эпизод, приемлемо по директиве.
6. **Пороги детекции не откалиброваны на живых релизах** (MinRMS, MinProminence, ±80 мс — стартовые): константы экспортированы; live-проба сверяет с ручным значением владельца.
7. **Сигнатурный ripple DownloadService.Download** (+sync): один прод-имплементор + фейк `droppingDownload`.
8. **Мульти-донорная верификация**: VerifyAssembled проверяет дорожку 0:a:0 (первый sync-трек); катастрофа на второй дорожке не ловится в v1 (лог per-track остаётся).
9. **Контейнер синхронизированных файлов всегда .mkv** (в т.ч. одиночный дубляж, который раньше давал .mp4) — следствие mkvmerge-архитектуры владельца; отражено в example.toml и строках отчёта. (**Veto-able design call.**)
10. **AAC-прайминг ±21–23 мс**: snap глушит; допуск верификации 1 c покрывает с запасом.

## 5. OWNER QUESTIONS

Открытых вопросов **не осталось**: Q1 (background → diagnostics-only) принят; Q2 аннулирован пивотом (коррелятор — единственный источник истины; ручной таблицы для будущих загрузок нет). Два дизайн-решения помечены в рисках как **veto-able** (пол #2: без `--version`-пробы mkvmerge; пол #9: sync-файлы всегда .mkv) — возражение в любой момент меняет одну строчку.
