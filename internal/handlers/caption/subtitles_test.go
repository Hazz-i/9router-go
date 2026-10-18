// Formatter unit tests: chunkWords boundaries and byte-exact SRT/ASS output.
package caption

import "testing"

func TestChunkWordsBoundaries(t *testing.T) {
	word := func(w string, start, end float64) Word {
		return Word{Word: w, Start: start, End: end, IsFinal: true}
	}

	t.Run("empty input yields no chunks", func(t *testing.T) {
		if got := ChunkWords(nil); len(got) != 0 {
			t.Errorf("ChunkWords(nil) = %v, want empty", got)
		}
	})

	t.Run("five words fill one chunk and the sixth starts a new one", func(t *testing.T) {
		words := []Word{
			word("a", 0, 0.1), word("b", 0.1, 0.2), word("c", 0.2, 0.3),
			word("d", 0.3, 0.4), word("e", 0.4, 0.5), word("f", 0.5, 0.6),
		}
		chunks := ChunkWords(words)
		if len(chunks) != 2 {
			t.Fatalf("len(chunks) = %d, want 2 (%v)", len(chunks), chunks)
		}
		if chunks[0].Text != "a b c d e" || chunks[0].Start != 0 || chunks[0].End != 0.5 {
			t.Errorf("first chunk = %v", chunks[0])
		}
		if chunks[1].Text != "f" || chunks[1].Start != 0.5 || chunks[1].End != 0.6 {
			t.Errorf("second chunk = %v", chunks[1])
		}
	})

	t.Run("a span over 2.5 s breaks the chunk", func(t *testing.T) {
		words := []Word{
			word("a", 0, 1.0), word("b", 1.0, 2.0), word("c", 2.0, 3.2),
		}
		chunks := ChunkWords(words)
		if len(chunks) != 2 {
			t.Fatalf("len(chunks) = %d, want 2 (%v)", len(chunks), chunks)
		}
		if chunks[0].Text != "a b" || chunks[0].End != 2.0 {
			t.Errorf("first chunk = %v, want \"a b\" ending 2.0", chunks[0])
		}
		if chunks[1].Text != "c" || chunks[1].Start != 2.0 {
			t.Errorf("second chunk = %v", chunks[1])
		}
	})

	t.Run("a gap over 0.8 s breaks the chunk", func(t *testing.T) {
		words := []Word{word("a", 0, 0.5), word("b", 1.5, 2.0)}
		chunks := ChunkWords(words)
		if len(chunks) != 2 {
			t.Fatalf("len(chunks) = %d, want 2 (%v)", len(chunks), chunks)
		}
		if chunks[0].Text != "a" || chunks[1].Text != "b" {
			t.Errorf("chunks = %v", chunks)
		}
		if chunks[0].End != 0.5 || chunks[1].Start != 1.5 {
			t.Errorf("chunk timing = %v", chunks)
		}
	})

	t.Run("trailing punctuation breaks before the next word", func(t *testing.T) {
		words := []Word{word("Hi.", 0, 0.4), word("there", 0.5, 0.9)}
		chunks := ChunkWords(words)
		if len(chunks) != 2 {
			t.Fatalf("len(chunks) = %d, want 2 (%v)", len(chunks), chunks)
		}
		if chunks[0].Text != "Hi." || chunks[1].Text != "there" {
			t.Errorf("chunks = %v", chunks)
		}
	})
}

func TestChunksToSrt(t *testing.T) {
	chunks := []Chunk{
		{Text: "halo dunia", Start: 0, End: 1.5},
		{Text: "satu.", Start: 2, End: 2.5},
	}
	want := "1\n00:00:00,000 --> 00:00:01,500\nhalo dunia\n\n2\n00:00:02,000 --> 00:00:02,500\nsatu.\n"
	if got := ChunksToSrt(chunks); got != want {
		t.Errorf("ChunksToSrt = %q, want %q", got, want)
	}
	if got := ChunksToSrt(nil); got != "" {
		t.Errorf("ChunksToSrt(nil) = %q, want empty", got)
	}
}

func TestChunksToAss(t *testing.T) {
	chunks := []Chunk{
		{
			Text: "halo dunia", Start: 0, End: 0.8,
			Words: []Word{
				{Word: "halo", Start: 0, End: 0.5},
				{Word: "dunia", Start: 0.5, End: 0.8},
			},
		},
	}
	got := ChunksToAss(chunks)
	want := assHeader + "Dialogue: 0,0:00:00.00,0:00:00.80,Default,,0,0,0,,{\\k50}halo {\\k30}dunia\n"
	if got != want {
		t.Errorf("ChunksToAss = %q, want %q", got, want)
	}

	t.Run("a chunk without words renders plain text", func(t *testing.T) {
		got := ChunksToAss([]Chunk{{Text: "halo", Start: 0, End: 0.4}})
		want := assHeader + "Dialogue: 0,0:00:00.00,0:00:00.40,Default,,0,0,0,,halo\n"
		if got != want {
			t.Errorf("ChunksToAss = %q, want %q", got, want)
		}
	})
}

func TestTimeFormatters(t *testing.T) {
	if got := FormatTimeSrt(3661.5); got != "01:01:01,500" {
		t.Errorf("FormatTimeSrt(3661.5) = %q", got)
	}
	if got := FormatTimeAss(3661.05); got != "1:01:01.05" {
		t.Errorf("FormatTimeAss(3661.05) = %q", got)
	}
	if got := FormatTimeSrt(-1); got != "00:00:00,000" {
		t.Errorf("FormatTimeSrt(-1) = %q, want clamped zero", got)
	}
}
