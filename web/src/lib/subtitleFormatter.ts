export interface CaptionWord {
  word: string
  start: number
  end: number
  confidence?: number
}

export interface SubtitleChunk {
  text: string
  start: number
  end: number
  words: CaptionWord[]
}

export function formatTimeSrt(seconds: number): string {
  const value = Math.max(0, Number(seconds) || 0)
  const hours = Math.floor(value / 3600)
  const minutes = Math.floor((value % 3600) / 60)
  const secs = Math.floor(value % 60)
  const milliseconds = Math.floor((value % 1) * 1000)
  return `${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(secs).padStart(2, '0')},${String(milliseconds).padStart(3, '0')}`
}

export function formatTimeAss(seconds: number): string {
  const value = Math.max(0, Number(seconds) || 0)
  const hours = Math.floor(value / 3600)
  const minutes = Math.floor((value % 3600) / 60)
  const secs = Math.floor(value % 60)
  const centiseconds = Math.floor((value % 1) * 100)
  return `${hours}:${String(minutes).padStart(2, '0')}:${String(secs).padStart(2, '0')}.${String(centiseconds).padStart(2, '0')}`
}

export function chunkWords(words: CaptionWord[]): SubtitleChunk[] {
  const chunks: SubtitleChunk[] = []
  let current: CaptionWord[] = []
  let chunkStart = 0
  let lastEnd = 0

  for (const source of words) {
    const word = String(source.word || '').trim()
    if (!word) continue
    const start = Number(source.start) || 0
    const end = Number(source.end) || start + 0.3
    const normalized = { ...source, word, start, end }
    if (current.length === 0) {
      chunkStart = start
      current = [normalized]
      lastEnd = end
      continue
    }

    const previous = current[current.length - 1]
    if (current.length >= 5 || end - chunkStart > 2.5 || start - lastEnd > 0.8 || /[.?!]$/.test(previous.word)) {
      chunks.push({ text: current.map((item) => item.word).join(' '), start: chunkStart, end: lastEnd, words: [...current] })
      chunkStart = start
      current = [normalized]
    } else {
      current.push(normalized)
    }
    lastEnd = end
  }

  if (current.length) chunks.push({ text: current.map((item) => item.word).join(' '), start: chunkStart, end: lastEnd, words: [...current] })
  return chunks
}

export function chunksToSrt(chunks: SubtitleChunk[]): string {
  return chunks.map((chunk, index) => `${index + 1}\n${formatTimeSrt(chunk.start)} --> ${formatTimeSrt(chunk.end)}\n${chunk.text}\n`).join('\n')
}

export function chunksToAss(chunks: SubtitleChunk[], title = 'Live Caption'): string {
  const header = `[Script Info]\nTitle: ${title}\nScriptType: v4.00+\nWrapStyle: 0\nScaledBorderAndShadow: yes\nYCbCr Matrix: None\nPlayResX: 1080\nPlayResY: 1920\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,Arial,60,&H00FFFFFF,&H0000FFFF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,3,2,2,40,40,240,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n`
  const events = chunks.map((chunk) => {
    const karaoke = chunk.words.length
      ? chunk.words.map((word) => `{\\k${Math.max(1, Math.round(((word.end - word.start) || 0.2) * 100))}}${word.word}`).join(' ')
      : chunk.text
    return `Dialogue: 0,${formatTimeAss(chunk.start)},${formatTimeAss(chunk.end)},Default,,0,0,0,,${karaoke}`
  })
  return header + (events.length ? `${events.join('\n')}\n` : '')
}
