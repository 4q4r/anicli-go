package providers

// AllAnime build-id material auto-derivation (PR70): a structural parser
// that recovers the client-crypto constants (dm table, salt/frag scalars,
// bootPrefix, x-aa-boot param order, buildId) straight from the LIVE
// player chunk — no JS engine, no manual re-port on rotation.
//
// Why this exists: the site rotates the obfuscated crypto chunk on its
// own schedule (buildId 168→173 in PR45, →174 with rotated mask
// constants live 2026-09-19; pinned-173 grace ends 2026-09-25 per the
// live switchAt). Owner ruling: the build-id material must update
// AUTOMATICALLY. Feasibility was proven against both live generations:
// the chunk is an obfuscator.io-style bundle whose string tables are
// plain fragment arrays accessed through offset arithmetic and aligned
// at load by a checksum-driven rotate loop — all texturally evaluable:
//
//	fragment table : function N(){const e=[...];return N=function(){return e},N()}
//	decoder        : function N(e[,t]){return e=e-OFFSET,TABLE()[e]}
//	wrapper        : function N(a[,b]){return DECODER(USED-DELTA)}   (DELTA numeric or {K:V}.K)
//	rotate IIFE    : (function(e,t){...parseInt(...)/D...===t...push(shift())})(TABLE,SEED)
//	constants obj  : {v:1,saltMul:N,saltAdd:N,fragMul:N,fragAdd:N,bootPrefix:E,join:S,
//	                  parts:[part,...],omitEmptyLane:B,envXor:N}
//	dm table       : NAME=[E,E,E,E] (4 x decoder-call concatenations) near the constants obj
//	buildId        : default-param var of the mask fn (my(e=cd) / gy(e=fd)), a decoder call
//
// The parser resolves the string expressions by simulating the rotate
// IIFEs (JS parseInt semantics, left-rotation until the checksum hits
// the seed) and then evaluating the constants expressions against the
// aligned tables. Validated [LIVE-VERIFIED 2026-09-19]:
//   - the 173 chunk (testdata/allanime/crypto_chunk_173_DhCxOiZl.js)
//     reproduces the pinned PR45 constants and mask byte-for-byte;
//   - the 174 chunk (crypto_chunk_174_BYlv1dKC.js) yields
//     saltMul=78/saltAdd=11, fragMul=100/fragAdd=58, envXor=167,
//     bootPrefix="3HZDdfe:", parts [buildId lane epoch host group]
//     (the param-string ORDER rotated with the build!), a fresh dm
//     table and buildId "174" — and the live bootstrap answered
//     HTTP 200 to an x-aa-boot built from this parsed material
//     (epoch 2959, switchAt 1790294400000 = 2026-09-25).

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// aaCryptoProfile is a complete set of bootstrap-derivation material:
// everything the x-aa-boot chain needs for one generation of the chunk.
type aaCryptoProfile struct {
	BuildID string
	// FixedMask, when set, is used directly (the browser-bridge handoff
	// path); the table fields below are then informational.
	FixedMask []byte
	// DM is the cy() base table: four 8-byte blocks.
	DM [4][8]byte
	// SaltMul/SaltAdd seed the buildId chars, FragMul/FragAdd salt the
	// mask blocks (the live cy formula, stable across both generations).
	SaltMul, SaltAdd, FragMul, FragAdd int
	// BootPrefix prefixes the first HMAC message (nT(buildId)).
	BootPrefix string
	// Parts is the x-aa-boot param-string field ORDER (the live aT
	// joins these names with Join); the 174 generation reordered it.
	Parts [5]string
	Join  string
}

// aaPartNames are the semantic x-aa-boot parameter names; every chunk
// generation must present exactly these five in some order.
var aaPartNames = [5]string{"lane", "epoch", "host", "group", "buildId"}

// aaPinnedProfile builds the last-resort profile from the Go-ported
// constants (allanime_proto.go). It bootstraps only while the pinned
// generation is still in the server's grace window.
func aaPinnedProfile(buildID string) *aaCryptoProfile {
	prof := &aaCryptoProfile{
		BuildID:    buildID,
		SaltMul:    aaSaltMul,
		SaltAdd:    aaSaltAdd,
		FragMul:    aaFragMul,
		FragAdd:    aaFragAdd,
		BootPrefix: aaBootPrefix,
		Parts:      [5]string{"lane", "epoch", "host", "group", "buildId"},
		Join:       "+",
	}
	for i := range prof.DM {
		copy(prof.DM[i][:], aaDMDelayed[i][:])
	}
	return prof
}

// aaProfileMask returns the 32-byte mask for the profile's buildId:
// the fixed bridge mask when set, otherwise the table fold.
func (p *aaCryptoProfile) aaProfileMask() ([]byte, error) {
	if p.FixedMask != nil {
		return p.FixedMask, nil
	}
	dm := p.DM
	return aaMaskWith(&dm, p.SaltMul, p.SaltAdd, p.FragMul, p.FragAdd, p.BuildID)
}

// aaProfileBootMessage ports nT(buildId) for this generation.
func (p *aaCryptoProfile) aaProfileBootMessage() string {
	return p.BootPrefix + p.BuildID
}

// aaProfileParamString ports aT(params) for this generation: the five
// values joined in the parsed order with the parsed separator. Unknown
// part names fail loudly (formula drift beyond the parsed shape).
func (p *aaCryptoProfile) aaProfileParamString(lane, epoch, host, group string) (string, error) {
	values := map[string]string{
		"lane":    lane,
		"epoch":   epoch,
		"host":    host,
		"group":   group,
		"buildId": p.BuildID,
	}
	pieces := make([]string, 0, len(p.Parts))
	for _, name := range p.Parts {
		v, ok := values[name]
		if !ok {
			return "", fmt.Errorf("allanime chunk profile: unknown param part %q", name)
		}
		pieces = append(pieces, v)
	}
	if len(pieces) != len(aaPartNames) {
		return "", fmt.Errorf("allanime chunk profile: param parts %v do not cover %v", p.Parts, aaPartNames)
	}
	return strings.Join(pieces, p.Join), nil
}

// aaValidate checks the structural invariants a parsed profile must
// hold (a garbage parse from an unaligned table must not reach the
// bootstrap).
func (p *aaCryptoProfile) aaValidate() error {
	if p.BuildID == "" {
		return fmt.Errorf("allanime chunk profile: empty buildId")
	}
	for _, r := range p.BuildID {
		if r < '0' || r > '9' {
			return fmt.Errorf("allanime chunk profile: buildId %q is not numeric", p.BuildID)
		}
	}
	if p.BootPrefix == "" || !strings.HasSuffix(p.BootPrefix, ":") {
		return fmt.Errorf("allanime chunk profile: bootPrefix %q lacks the suffix shape", p.BootPrefix)
	}
	if p.Join == "" {
		return fmt.Errorf("allanime chunk profile: empty join")
	}
	seen := map[string]bool{}
	for _, name := range p.Parts {
		seen[name] = true
	}
	if len(seen) != len(aaPartNames) {
		return fmt.Errorf("allanime chunk profile: parts %v incomplete", p.Parts)
	}
	return nil
}

// aaMaskWith is the generation-independent cy() table fold (the same
// formula as aaMask, parameterized): one code path serves both the
// pinned constants and parsed chunk tables.
func aaMaskWith(dm *[4][8]byte, saltMul, saltAdd, fragMul, fragAdd int, buildID string) ([]byte, error) {
	if buildID == "" {
		return nil, errAAMaskBuildID
	}
	codes := utf16.Encode([]rune(buildID))
	mask := make([]byte, 32)
	for l := range 32 {
		var code int
		if len(codes) > 0 {
			code = int(codes[l%len(codes)])
		}
		seed := byte(code) ^ byte((l*saltMul+saltAdd)&0xff)
		block, off := l/8, l%8
		mask[l] = dm[block][off] ^ seed ^ byte((block*fragMul+off*fragAdd)&0xff)
	}
	return mask, nil
}

// ---------------------------------------------------------------------------
// Chunk parsing. The regexes mirror the obfuscator output shapes observed
// in both live generations; where Go RE2 lacks backreferences the equality
// is checked at runtime.
// ---------------------------------------------------------------------------

var (
	// aaTableRe: function N(){const v=[...];return N=function(){return v},N()}
	aaTableRe = regexp.MustCompile(
		`(?s)function (\w+)\(\)\{const \w+=\[(.*?)\];return (\w+)=function\(\)\{return \w+\},\w+\(\)\}`)
	// aaDecoderRe: function N(e[,t]){return e=e-OFFSET,TABLE()[e]}
	aaDecoderRe = regexp.MustCompile(
		`function (\w+)\(\w+(?:,\w+)?\)\{return \w+=(\w+)-((?:\{(\w+):(\d+)\}\.\w+|[\w+\-*/() ]+?)),(\w+)\(\)\[\w+\]\}`)
	// aaWrapRe: function N(a[,b]){return DECODER(USED-DELTA)}; DELTA is a
	// plain (possibly negative) number or an inline-object constant.
	aaWrapRe = regexp.MustCompile(
		`function (\w+)\((\w+)(?:,(\w+))?\)\{return (\w+)\((\w+)-(?:\{(\w+):(\d+)\}\.\w+|(\s*-?\d+))\)\}`)
	// aaIIFECallRe: the rotate-IIFE invocation tail: })(TABLE,SEED);
	aaIIFECallRe = regexp.MustCompile(`\}\)\((\w+),([-+\d*\s]+?)\);`)
	// aaNanRe: unresolved fragment markers in checksum arithmetic
	// (hoisted — aaEvalChecksum runs per rotation attempt per table and
	// a MustCompile here recompiled the same literal thousands of times
	// per chunk parse; PR81 review #2).
	aaNanRe = regexp.MustCompile(`\bnan\b`)
	// aaIIFEHeadRe anchors the IIFE body for a call site.
	aaIIFEHeadRe = regexp.MustCompile(`\(function\(\w+,\w+\)\{`)
	// aaChecksumHeadRe anchors the checksum expression inside the IIFE.
	aaChecksumHeadRe = regexp.MustCompile(`for\(;;\)try\{if\(`)
	// aaLocalWrapRe: IIFE-local wrapper defs (same shape as aaWrapRe).
	aaLocalWrapRe = aaWrapRe
	// aaConstRefRe: `_0xNAME` references (optionally const-prefixed/negated).
	aaConstRefRe = regexp.MustCompile(`^-?(?:\w+\.)?(_0x\w+)$`)
	// aaConstObjRe: the crypto constants object (both generations).
	aaConstObjRe = regexp.MustCompile(
		`\{v:1,saltMul:(-?\d+),saltAdd:(-?\d+),fragMul:(-?\d+),fragAdd:(-?\d+),` +
			`bootPrefix:((?:\([^()]*\)|[^,])+),join:("(?:[^"\\])*"),parts:\[([^\]]*)\],` +
			`omitEmptyLane:(!0|!1),envXor:(-?\d+)\}`)
	// aaMaskFnRe: the mask function's default-param buildId reference
	// (my(e=cd) in 173, gy(e=fd) in 174).
	aaMaskFnRe = regexp.MustCompile(`function \w+\(e=(\w+)\)`)
	// aaChunkCallRe: decoder-call | string literal inside expressions.
	aaChunkCallRe = regexp.MustCompile(`(\w+)\((-?\d+)(?:,(-?\d+))?\)|"([^"\\]*)"`)
	// aaVarCallRe: VAR=DECODER(args) — the buildId variable's definition.
	aaVarCallRe = regexp.MustCompile(`(\w+)=(\w+)\((-?\d+)(?:,(-?\d+))?\)`)
	// aaDMTableRe: NAME=[E,E,E,E] candidates for the dm table (the
	// continuation check RE2 cannot express is done at runtime).
	aaDMTableRe = regexp.MustCompile(`(\w+)=\[([^\[\]]+)\]`)
	// aaCallCountRe: decoder-call heads inside a dm-table entry.
	aaCallCountRe = regexp.MustCompile(`\w+\(-?\d+`)
	// aaCryptoChunkMarker is the stable ST error literal identifying the
	// crypto chunk among the entry bundle's imports (same marker the
	// browser bridge hunts for).
	aaCryptoChunkMarker = "invalid_part_b"
)

// aaParseInt ports JS parseInt for the checksum feeder strings: the
// leading integer prefix, ok=false (NaN) when there are no digits.
func aaParseInt(s string) (float64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == digits {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[start:i], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// aaChunkNan is unused: NaN is carried inline by the evaluator tokens.

// aaArithEval evaluates the integer arithmetic the obfuscator emits for
// decoder offsets and IIFE seeds: ints, + - * /, parens, unary minus,
// NaN propagation — JS semantics, float64 carrier.
func aaArithEval(expr string) (float64, error) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, expr)
	if s == "" {
		return 0, fmt.Errorf("allanime chunk: empty arithmetic")
	}
	type tok struct {
		num float64
		op  byte
		nan bool
	}
	var toks []tok
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c >= '0' && c <= '9' || c == '.':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			f, err := strconv.ParseFloat(s[i:j], 64)
			if err != nil {
				return 0, fmt.Errorf("allanime chunk: bad number in %q", expr)
			}
			toks = append(toks, tok{num: f})
			i = j
		case strings.HasPrefix(s[i:], "nan"):
			toks = append(toks, tok{nan: true})
			i += 3
		case strings.ContainsRune("()+-*/", rune(c)):
			toks = append(toks, tok{op: c})
			i++
		default:
			return 0, fmt.Errorf("allanime chunk: unexpected %q in %q", string(c), expr)
		}
	}
	pos := 0
	var primary, mulDiv, addSub func() (float64, error)
	primary = func() (float64, error) {
		if pos >= len(toks) {
			return 0, fmt.Errorf("allanime chunk: unexpected end of %q", expr)
		}
		t := toks[pos]
		pos++
		if t.nan {
			return t.num, nil
		}
		if t.op == 0 {
			return t.num, nil
		}
		switch t.op {
		case '(':
			v, err := addSub()
			if err != nil {
				return 0, err
			}
			if pos >= len(toks) || toks[pos].op != ')' {
				return 0, fmt.Errorf("allanime chunk: unbalanced parens in %q", expr)
			}
			pos++
			return v, nil
		case '-':
			v, err := primary()
			return -v, err
		case '+':
			return primary()
		}
		return 0, fmt.Errorf("allanime chunk: unexpected token %q in %q", string(t.op), expr)
	}
	mulDiv = func() (float64, error) {
		v, err := primary()
		if err != nil {
			return 0, err
		}
		for pos < len(toks) && toks[pos].op != 0 && (toks[pos].op == '*' || toks[pos].op == '/') {
			op := toks[pos].op
			pos++
			r, err := primary()
			if err != nil {
				return 0, err
			}
			if op == '*' {
				v *= r
			} else {
				v /= r
			}
		}
		return v, nil
	}
	addSub = func() (float64, error) {
		v, err := mulDiv()
		if err != nil {
			return 0, err
		}
		for pos < len(toks) && toks[pos].op != 0 && (toks[pos].op == '+' || toks[pos].op == '-') {
			op := toks[pos].op
			pos++
			r, err := mulDiv()
			if err != nil {
				return 0, err
			}
			if op == '+' {
				v += r
			} else {
				v -= r
			}
		}
		return v, nil
	}
	v, err := addSub()
	if err != nil {
		return 0, err
	}
	if pos != len(toks) {
		return 0, fmt.Errorf("allanime chunk: trailing tokens in %q", expr)
	}
	return v, nil
}

// aaChunkFragments extracts the string fragments of one table body.
func aaChunkFragments(body string) []string {
	lit := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'`)
	var frags []string
	for _, m := range lit.FindAllStringSubmatch(body, -1) {
		if m[1] != "" || m[2] == "" {
			frags = append(frags, aaUnquote(m[1]))
		} else {
			frags = append(frags, aaUnquote(m[2]))
		}
	}
	return frags
}

// aaUnquote decodes the escape set the obfuscator emits (\", \', \\,
// \n, \t, \uXXXX); anything else passes through verbatim.
func aaUnquote(raw string) string {
	if !strings.Contains(raw, "\\") {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '\\' || i+1 >= len(raw) {
			b.WriteByte(raw[i])
			i++
			continue
		}
		switch raw[i+1] {
		case '\\', '"', '\'':
			b.WriteByte(raw[i+1])
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'u':
			if i+6 <= len(raw) {
				if v, err := strconv.ParseUint(raw[i+2:i+6], 16, 32); err == nil && v <= 0xFFFF {
					b.WriteRune(rune(v))
					i += 6
					continue
				}
			}
			b.WriteByte(raw[i])
			i++
		default:
			b.WriteByte(raw[i])
			i++
		}
	}
	return b.String()
}

// aaChunkDecoder is one table decoder: TABLE()[idx - offset].
type aaChunkDecoder struct {
	table  string
	offset float64
}

// aaChunkWrapper is one wrapper: DECODER(args[pos] - delta).
type aaChunkWrapper struct {
	pos    int // which parameter (0-based) feeds the index
	target string
	delta  float64
	off    int // source offset, nearest-anchor resolution
}

// aaChunkTables is the parsed decoder machinery of one chunk.
type aaChunkTables struct {
	src      string
	tables   map[string][]string
	decoders map[string]aaChunkDecoder
	wrappers map[string][]aaChunkWrapper
	failures []string // rotation-simulation diagnostics (non-fatal tables)
}

// aaParseChunkTables extracts tables, decoders and wrappers; the
// mega-chunk embeds the crypto module twice, so decoders/wrappers keep
// every occurrence and the resolver picks the one nearest the anchor.
func aaParseChunkTables(src string) (*aaChunkTables, error) {
	ct := &aaChunkTables{
		src:      src,
		tables:   map[string][]string{},
		decoders: map[string]aaChunkDecoder{},
		wrappers: map[string][]aaChunkWrapper{},
	}
	for _, m := range aaTableRe.FindAllStringSubmatch(src, -1) {
		if m[1] != m[3] {
			continue // shape invariant: the getter memoizes under its own name
		}
		ct.tables[m[1]] = aaChunkFragments(m[2])
	}
	for _, loc := range aaDecoderRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		// JS rebind `return e=e-OFFSET,TABLE()[e]`: group 3 is the
		// offset arithmetic — table index = call index - offset.
		arith := src[loc[6]:loc[7]]
		tableName := src[loc[12]:loc[13]]
		offset, err := aaArithEval(arith)
		if err != nil {
			continue
		}
		ct.decoders[name] = aaChunkDecoder{table: tableName, offset: offset}
	}
	for _, m := range aaWrapRe.FindAllStringSubmatch(src, -1) {
		w := aaChunkWrapper{target: m[4], off: strings.Index(src, m[0])}
		switch {
		case m[8] != "":
			w.delta, _ = aaArithEval(m[8])
		case m[6] != "" && m[7] != "":
			w.delta, _ = aaArithEval(m[7])
		default:
			continue
		}
		param := m[5]
		switch {
		case param == m[2]:
			w.pos = 0
		case m[3] != "" && param == m[3]:
			w.pos = 1
		default:
			continue // the used param must be one of the declared params
		}
		ct.wrappers[m[1]] = append(ct.wrappers[m[1]], w)
	}
	if len(ct.tables) == 0 || len(ct.decoders) == 0 {
		return nil, fmt.Errorf("allanime chunk: decoder machinery not found")
	}
	return ct, nil
}

// aaSimulateRotations replays every rotate IIFE bound to a known table:
// left-rotation (push(shift())) until the parseInt checksum equals the
// seed, bounded by the table length. Failures on unrelated modules'
// tables are recorded (diagnostics) — the constants parse is the loud
// validator for the tables the crypto code actually uses.
func (ct *aaChunkTables) aaSimulateRotations() {
	for _, loc := range aaIIFECallRe.FindAllStringSubmatchIndex(ct.src, -1) {
		name := ct.src[loc[2]:loc[3]]
		if _, ok := ct.tables[name]; !ok {
			continue
		}
		seed, err := aaArithEval(ct.src[loc[4]:loc[5]])
		if err != nil {
			ct.failures = append(ct.failures, fmt.Sprintf("rotation %s: seed: %v", name, err))
			continue
		}
		if err := ct.aaRunRotateIIFE(loc[0], name, seed); err != nil {
			ct.failures = append(ct.failures, err.Error())
		}
	}
}

// aaRunRotateIIFE simulates one IIFE: extract its body (nearest
// `(function(e,t){` head before the call site), parse the local consts
// and wrapper aliases, then rotate until the checksum matches.
func (ct *aaChunkTables) aaRunRotateIIFE(callStart int, name string, seed float64) error {
	lo := callStart - 3000
	if lo < 0 {
		lo = 0
	}
	seg := ct.src[lo:callStart]
	heads := aaIIFEHeadRe.FindAllStringIndex(seg, -1)
	if len(heads) == 0 {
		return fmt.Errorf("rotation %s: IIFE head not found", name)
	}
	body := seg[heads[len(heads)-1][0]:]
	end := strings.Index(body, "===t)break")
	if end < 0 {
		return fmt.Errorf("rotation %s: checksum compare not found", name)
	}
	cm := aaChecksumHeadRe.FindStringIndex(body)
	if cm == nil {
		return fmt.Errorf("rotation %s: checksum head not found", name)
	}
	expr := body[cm[1]:end]

	// local `_0xNAME:NUM` const table
	consts := map[string]string{}
	for _, m := range regexp.MustCompile(`(_0x\w+):(-?\d+)[,}]`).FindAllStringSubmatch(body, -1) {
		consts[m[1]] = m[2]
	}
	// local wrappers (same shape as module wrappers)
	local := map[string]aaChunkWrapper{}
	for _, m := range aaLocalWrapRe.FindAllStringSubmatch(body, -1) {
		w := aaChunkWrapper{target: m[4]}
		switch {
		case m[8] != "":
			w.delta, _ = aaArithEval(m[8])
		case m[6] != "" && m[7] != "":
			w.delta, _ = aaArithEval(m[7])
		default:
			continue
		}
		param := m[5]
		switch {
		case param == m[2]:
			w.pos = 0
		case m[3] != "" && param == m[3]:
			w.pos = 1
		default:
			continue
		}
		local[m[1]] = w
	}
	// local decoder aliases: const r=DecoderName
	aliases := map[string]string{}
	for _, m := range regexp.MustCompile(`const (\w+)=(\w+)[,;]`).FindAllStringSubmatch(body, -1) {
		if _, ok := ct.decoders[m[2]]; ok {
			aliases[m[1]] = m[2]
		}
	}

	constCall := regexp.MustCompile(`parseInt\((\w+)\(([^()]*)\)\)`)
	n := len(ct.tables[name])
	for range n + 1 {
		val, err := ct.aaEvalChecksum(expr, constCall, local, aliases, consts, name)
		if err == nil && val == seed {
			return nil // aligned at this shift
		}
		f := ct.tables[name]
		ct.tables[name] = append(append([]string{}, f[1:]...), f[0])
	}
	return fmt.Errorf("rotation %s: checksum never matched (seed %v, %d fragments)", name, seed, n)
}

// aaEvalChecksum substitutes every parseInt(WRAPPER(args)) with the
// parseInt of the resolved fragment and evaluates the arithmetic.
func (ct *aaChunkTables) aaEvalChecksum(expr string, constCall *regexp.Regexp, local map[string]aaChunkWrapper, aliases map[string]string, consts map[string]string, table string) (float64, error) {
	filled := constCall.ReplaceAllStringFunc(expr, func(call string) string {
		m := constCall.FindStringSubmatch(call)
		fnName, argStr := m[1], m[2]
		args := strings.Split(argStr, ",")
		var target string
		var idx float64
		resolveArg := func(raw string) (float64, bool) {
			a := strings.TrimSpace(raw)
			if cm := aaConstRefRe.FindStringSubmatch(a); cm != nil {
				neg := strings.HasPrefix(a, "-")
				v, ok := consts[cm[1]]
				if !ok {
					return 0, false
				}
				if neg {
					v = "-" + v
				}
				a = v
			}
			f, err := strconv.ParseFloat(a, 64)
			if err != nil {
				return 0, false
			}
			return f, true
		}
		if w, ok := local[fnName]; ok {
			pos := w.pos
			if pos >= len(args) {
				pos = 0
			}
			a, ok := resolveArg(args[pos])
			if !ok {
				return "nan"
			}
			idx = a - w.delta
			target = w.target
		} else if alias, ok := aliases[fnName]; ok {
			a, ok := resolveArg(args[0])
			if !ok {
				return "nan"
			}
			idx = a
			target = alias
		} else {
			return "nan"
		}
		d, ok := ct.decoders[target]
		if !ok {
			return "nan"
		}
		frags := ct.tables[table]
		i := int(idx - d.offset)
		if i < 0 || i >= len(frags) {
			return "nan"
		}
		if v, ok := aaParseInt(frags[i]); ok {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return "nan"
	})
	filled = aaNanRe.ReplaceAllString(filled, "nan")
	return aaArithEval(filled)
}

// aaChunkResolve resolves one string expression (decoder-call and
// literal concatenation) against the aligned tables, anchoring decoder
// and wrapper resolution at the given source offset.
func (ct *aaChunkTables) aaChunkResolve(anchor int, expr string) (string, error) {
	nearestDecoder := func(name string) (aaChunkDecoder, bool) {
		d, ok := ct.decoders[name]
		return d, ok
	}
	nearestWrapper := func(name string) (aaChunkWrapper, bool) {
		list, ok := ct.wrappers[name]
		if !ok {
			return aaChunkWrapper{}, false
		}
		best := list[0]
		for _, w := range list[1:] {
			if absInt(w.off-anchor) < absInt(best.off-anchor) {
				best = w
			}
		}
		return best, true
	}
	var resolve func(fn string, args []float64) (string, error)
	resolve = func(fn string, args []float64) (string, error) {
		if d, ok := nearestDecoder(fn); ok {
			frags := ct.tables[d.table]
			i := int(args[0] - d.offset)
			if i < 0 || i >= len(frags) {
				return "", fmt.Errorf("allanime chunk: %s(%v) → table index %d out of range", fn, args, i)
			}
			return frags[i], nil
		}
		if w, ok := nearestWrapper(fn); ok {
			used := args[0]
			if w.pos < len(args) {
				used = args[w.pos]
			}
			return resolve(w.target, []float64{used - w.delta})
		}
		return "", fmt.Errorf("allanime chunk: decoder %q not found", fn)
	}

	var b strings.Builder
	pos := 0
	for _, m := range aaChunkCallRe.FindAllStringSubmatchIndex(expr, -1) {
		if strings.TrimSpace(expr[pos:m[0]]) != "" && strings.TrimSpace(expr[pos:m[0]]) != "+" {
			return "", fmt.Errorf("allanime chunk: bad joiner in %q", expr)
		}
		if m[2] >= 0 { // decoder call
			fn := expr[m[2]:m[3]]
			args := []float64{parseFloatMust(expr[m[4]:m[5]])}
			if m[6] >= 0 {
				args = append(args, parseFloatMust(expr[m[6]:m[7]]))
			}
			s, err := resolve(fn, args)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		} else { // string literal
			b.WriteString(expr[m[8]:m[9]])
		}
		pos = m[1]
	}
	if strings.TrimSpace(expr[pos:]) != "" {
		return "", fmt.Errorf("allanime chunk: trailing content in %q", expr)
	}
	return b.String(), nil
}

// aaSplitTop splits on depth-0 commas (parenthesis-aware — two-arg
// decoder calls like tr(153,245) must stay intact).
func aaSplitTop(s string) []string {
	var out []string
	var buf strings.Builder
	depth := 0
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				if v := strings.TrimSpace(buf.String()); v != "" {
					out = append(out, v)
				}
				buf.Reset()
				continue
			}
		}
		buf.WriteRune(ch)
	}
	if v := strings.TrimSpace(buf.String()); v != "" {
		out = append(out, v)
	}
	return out
}

func parseFloatMust(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func absInt(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// aaParseChunkMaterial runs the full pipeline over a chunk source and
// returns the validated crypto profile.
func aaParseChunkMaterial(src string) (*aaCryptoProfile, error) {
	ct, err := aaParseChunkTables(src)
	if err != nil {
		return nil, err
	}
	ct.aaSimulateRotations()

	cm := aaConstObjRe.FindStringIndex(src)
	if cm == nil {
		return nil, fmt.Errorf("allanime chunk: constants object not found (rotation failures: %v)", ct.failures)
	}
	obj := aaConstObjRe.FindStringSubmatch(src)
	anchor := cm[0]

	prof := &aaCryptoProfile{
		SaltMul: mustInt(obj[1]), SaltAdd: mustInt(obj[2]),
		FragMul: mustInt(obj[3]), FragAdd: mustInt(obj[4]),
		Join: unquoteJSONString(obj[6]),
	}
	if obj[8] == "!0" {
		return nil, fmt.Errorf("allanime chunk: omitEmptyLane=true unsupported (formula shape drifted)")
	}
	_ = mustInt(obj[9]) // envXor: informational (clean-browser path applies no XOR)

	bootPrefix, err := ct.aaChunkResolve(anchor, obj[5])
	if err != nil {
		return nil, fmt.Errorf("allanime chunk: bootPrefix: %w", err)
	}
	prof.BootPrefix = bootPrefix

	partsRaw := aaSplitTop(obj[7])
	if len(partsRaw) != len(prof.Parts) {
		return nil, fmt.Errorf("allanime chunk: %d param parts, want %d", len(partsRaw), len(prof.Parts))
	}
	for i, raw := range partsRaw {
		name, err := ct.aaChunkResolve(anchor, raw)
		if err != nil {
			return nil, fmt.Errorf("allanime chunk: part %d: %w", i, err)
		}
		prof.Parts[i] = name
	}

	// dm table: the nearest `NAME=[E,E,E,E]` before the constants object
	// whose entries are decoder-call concatenations.
	dmRaw, err := ct.aaFindDMTable(anchor)
	if err != nil {
		return nil, err
	}
	dmPieces := aaSplitTop(dmRaw)
	if len(dmPieces) != len(prof.DM) {
		return nil, fmt.Errorf("allanime chunk: dm table has %d entries, want %d", len(dmPieces), len(prof.DM))
	}
	for i, raw := range dmPieces {
		s, err := ct.aaChunkResolve(anchor, raw)
		if err != nil {
			return nil, fmt.Errorf("allanime chunk: dm[%d]: %w", i, err)
		}
		block, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(block) != 8 {
			return nil, fmt.Errorf("allanime chunk: dm[%d] %q is not an 8-byte base64 block", i, s)
		}
		copy(prof.DM[i][:], block)
	}

	// buildId: the default-param var of the mask function after the
	// constants object, defined as a decoder call before it.
	prof.BuildID, err = ct.aaFindBuildID(anchor)
	if err != nil {
		return nil, err
	}

	if err := prof.aaValidate(); err != nil {
		return nil, fmt.Errorf("allanime chunk: %w (rotation failures: %v)", err, ct.failures)
	}
	return prof, nil
}

// aaFindDMTable locates the dm table: the nearest `NAME=[E,E,E,E]`
// before the constants object whose entries are decoder-call chains.
func (ct *aaChunkTables) aaFindDMTable(anchor int) (string, error) {
	lo := anchor - 3000
	if lo < 0 {
		lo = 0
	}
	window := ct.src[lo:anchor]
	best := ""
	found := false
	for _, m := range aaDMTableRe.FindAllStringSubmatch(window, -1) {
		body := m[2]
		// the entry after the closing bracket must not continue the
		// identifier (RE2 has no lookahead; check manually)
		end := strings.Index(window, m[0]) + len(m[0])
		c := byte(0)
		if end < len(window) {
			c = window[end]
		}
		isIdentContinuation := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '[' || c == '('
		if isIdentContinuation {
			continue
		}
		calls := aaCallCountRe.FindAllString(body, -1)
		if len(calls) >= 8 && len(aaSplitTop(body)) == 4 {
			best = body
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("allanime chunk: dm table not found (rotation failures: %v)", ct.failures)
	}
	return best, nil
}

// aaFindBuildID resolves the mask function's default-param buildId: the
// variable is defined near the constants object as a decoder call.
func (ct *aaChunkTables) aaFindBuildID(anchor int) (string, error) {
	after := ct.src[anchor:]
	for _, m := range aaMaskFnRe.FindAllStringSubmatch(after, -1) {
		varName := m[1]
		// definition: VAR=DECODER(args...) before the constants object
		defs := aaVarCallRe.FindAllStringSubmatch(ct.src[:anchor], -1)
		for i := len(defs) - 1; i >= 0; i-- { // nearest definition wins
			d := defs[i]
			if d[1] != varName {
				continue
			}
			args := []float64{parseFloatMust(d[3])}
			if d[4] != "" {
				args = append(args, parseFloatMust(d[4]))
			}
			s, err := ct.aaChunkResolve(anchor, d[2]+"("+strings.Join(argStrings(args), ",")+")")
			if err != nil {
				continue // try the next definition
			}
			return s, nil
		}
	}
	return "", fmt.Errorf("allanime chunk: buildId variable not resolved")
}

// argStrings renders float args back to integer literals for re-matching.
func argStrings(args []float64) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strconv.Itoa(int(a))
	}
	return out
}

func mustInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return i
}

// unquoteJSONString decodes a plain double-quoted JSON-ish literal.
func unquoteJSONString(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return aaUnquote(s[1 : len(s)-1])
	}
	return s
}
