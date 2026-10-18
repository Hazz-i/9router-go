// Package caption ports src/sse/handlers/liveCaptionWs.js and
// src/sse/utils/subtitleFormatter.js (9router v0.5.105): the live-caption
// WebSocket session on /v1/audio/live-transcriptions.
//
// This file ports subtitleFormatter.js verbatim: word-rhythm chunking, SRT
// timestamps, and karaoke ASS events.
package caption

import (
	"fmt"
	"math"
	"strings"
)

// Word is one committed transcript word. It is both the session state and the
// wire shape of the `word` gateway frame (upstream spreads wordObj into it).
// Confidence is omitted when the upstream report carried none (Gemini).
type Word struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence,omitempty"`
	IsFinal    bool    `json:"isFinal"`
}

// Chunk is one subtitle segment: the `chunk` gateway frame payload and the
// `chunks` entries of the `subtitles` frame. Words is omitted for the
// ElevenLabs committed-span chunks, which upstream emits without it.
type Chunk struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Words []Word  `json:"words,omitempty"`
}

// Chunking limits upstream chunkWords defaults: maxWords 5, maxDuration 2.5 s,
// maxGap 0.8 s.
const (
	chunkMaxWords    = 5
	chunkMaxDuration = 2.5
	chunkMaxGap      = 0.8
)

// FormatTimeSrt renders seconds as an SRT timestamp HH:MM:SS,mmm.
func FormatTimeSrt(seconds float64) string {
	s := clampSeconds(seconds)
	hrs := int(s / 3600)
	mins := int(math.Mod(s, 3600) / 60)
	secs := int(math.Mod(s, 60))
	ms := int(math.Floor(math.Mod(s, 1) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hrs, mins, secs, ms)
}

// FormatTimeAss renders seconds as an ASS timestamp H:MM:SS.cc (centiseconds).
func FormatTimeAss(seconds float64) string {
	s := clampSeconds(seconds)
	hrs := int(s / 3600)
	mins := int(math.Mod(s, 3600) / 60)
	secs := int(math.Mod(s, 60))
	cs := int(math.Floor(math.Mod(s, 1) * 100))
	return fmt.Sprintf("%d:%02d:%02d.%02d", hrs, mins, secs, cs)
}

// clampSeconds mirrors `Math.max(0, Number(seconds) || 0)`: negatives and NaN
// collapse to zero.
func clampSeconds(seconds float64) float64 {
	if math.IsNaN(seconds) || seconds < 0 {
		return 0
	}
	return seconds
}

// ChunkWords groups words into readable subtitle segments, breaking on the
// word count, the total span, the inter-word gap, or trailing sentence
// punctuation. It is a faithful port of upstream chunkWords with the default
// options.
func ChunkWords(words []Word) []Chunk {
	if len(words) == 0 {
		return nil
	}

	var chunks []Chunk
	var current []Word
	var chunkStart, lastEnd float64

	for _, w := range words {
		wordText := strings.TrimSpace(w.Word)
		if wordText == "" {
			continue
		}
		start := w.Start
		end := w.End
		if end == 0 {
			end = start + 0.3
		}
		norm := w
		norm.Word, norm.Start, norm.End = wordText, start, end

		if len(current) == 0 {
			chunkStart, lastEnd = start, end
			current = append(current, norm)
			continue
		}

		durationSoFar := end - chunkStart
		gapFromLast := start - lastEnd
		hasPunctuation := endsWithPunct(current[len(current)-1].Word)

		if len(current) >= chunkMaxWords || durationSoFar > chunkMaxDuration ||
			gapFromLast > chunkMaxGap || hasPunctuation {
			chunks = append(chunks, Chunk{
				Text:  joinWords(current),
				Start: chunkStart,
				End:   lastEnd,
				Words: current,
			})
			chunkStart = start
			current = []Word{norm}
		} else {
			current = append(current, norm)
		}
		lastEnd = end
	}

	if len(current) > 0 {
		chunks = append(chunks, Chunk{
			Text:  joinWords(current),
			Start: chunkStart,
			End:   lastEnd,
			Words: current,
		})
	}
	return chunks
}

// joinWords renders a chunk's text the way upstream does: the (punctuated)
// words joined by single spaces.
func joinWords(words []Word) string {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = w.Word
	}
	return strings.Join(parts, " ")
}

func endsWithPunct(s string) bool {
	return strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!")
}

// ChunksToSrt renders chunks as an .srt document: numbered blocks with
// HH:MM:SS,mmm --> timecodes, blank-line separated (a trailing newline per
// block, joined by one more).
func ChunksToSrt(chunks []Chunk) string {
	if len(chunks) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range chunks {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n", i+1,
			FormatTimeSrt(c.Start), FormatTimeSrt(c.End), c.Text)
		if i < len(chunks)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// assHeader is upstream's fixed ASS preamble: karaoke-ready style, portrait
// PlayRes 1080x1920.
const assHeader = `[Script Info]
Title: Live Caption
ScriptType: v4.00+
WrapStyle: 0
ScaledBorderAndShadow: yes
YCbCr Matrix: None
PlayResX: 1080
PlayResY: 1920

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,60,&H00FFFFFF,&H0000FFFF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,3,2,2,40,40,240,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

// ChunksToAss renders chunks as an .ass document whose Dialogue lines carry
// per-word karaoke tags (\k<centiseconds>).
func ChunksToAss(chunks []Chunk) string {
	var b strings.Builder
	b.WriteString(assHeader)
	for _, c := range chunks {
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Default,,0,0,0,,%s\n",
			FormatTimeAss(c.Start), FormatTimeAss(c.End), karaokeText(c))
	}
	return b.String()
}

// karaokeText prefixes each word with its \k duration in centiseconds, or
// falls back to the plain chunk text when the chunk carries no words.
func karaokeText(c Chunk) string {
	if len(c.Words) == 0 {
		return c.Text
	}
	parts := make([]string, len(c.Words))
	for i, w := range c.Words {
		dur := w.End - w.Start
		if dur == 0 {
			dur = 0.2
		}
		cs := int(math.Round(dur * 100))
		if cs < 1 {
			cs = 1
		}
		parts[i] = fmt.Sprintf("{\\k%d}%s", cs, w.Word)
	}
	return strings.Join(parts, " ")
}
