import { describe, expect, it } from 'bun:test'
import { chunkWords, chunksToAss, chunksToSrt, type CaptionWord } from './subtitleFormatter'

const fixedWords: CaptionWord[] = [
  { word: 'one', start: 0, end: 0.25 },
  { word: 'two', start: 0.3, end: 0.55 },
  { word: 'three', start: 0.6, end: 0.85 },
  { word: 'four', start: 0.9, end: 1.15 },
  { word: 'five.', start: 1.2, end: 1.45 },
  { word: 'six', start: 1.5, end: 1.75 },
]

describe('chunkWords', () => {
  it.each([
    ['five word limit', fixedWords, ['one two three four five.', 'six']],
    ['2.5 second span', [{ word: 'one', start: 0, end: 0.3 }, { word: 'two', start: 1, end: 1.3 }, { word: 'three', start: 2.6, end: 2.9 }], ['one two', 'three']],
    ['0.8 second gap', [{ word: 'one', start: 0, end: 0.3 }, { word: 'two', start: 1.11, end: 1.4 }], ['one', 'two']],
    ['sentence punctuation', [{ word: 'Ready?', start: 0, end: 0.3 }, { word: 'Go', start: 0.4, end: 0.7 }], ['Ready?', 'Go']],
  ] as Array<[string, CaptionWord[], string[]]>)('breaks on %s', (_name, words, expected) => {
    expect(chunkWords(words).map((chunk) => chunk.text)).toEqual(expected)
  })
})

describe('subtitle outputs', () => {
  const chunks = chunkWords(fixedWords)

  it('formats SRT timestamps and sequence numbers', () => {
    expect(chunksToSrt(chunks)).toBe('1\n00:00:00,000 --> 00:00:01,449\none two three four five.\n\n2\n00:00:01,500 --> 00:00:01,750\nsix\n')
  })

  it('formats ASS karaoke with portrait play resolution', () => {
    const ass = chunksToAss(chunks)
    expect(ass).toContain('PlayResX: 1080\nPlayResY: 1920')
    expect(ass).toContain('Dialogue: 0,0:00:00.00,0:00:01.44,Default,,0,0,0,,{\\k25}one {\\k25}two {\\k25}three {\\k25}four {\\k25}five.')
    expect(ass).toContain('Dialogue: 0,0:00:01.50,0:00:01.75,Default,,0,0,0,,{\\k25}six')
  })
})
