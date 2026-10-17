import type { IReferenceChunk } from '@/interfaces/database/chat';
import {
  buildCitationDisplayMap,
  citationMarkerReg,
  normalizeCitationMarkers,
  parseCitationIndex,
  resolveCitationChunk,
} from '../citation-utils';

const buildChunk = (id: string): IReferenceChunk => ({
  id,
  content: null,
  document_id: `doc-${id}`,
  document_name: `${id}.txt`,
  dataset_id: 'kb-1',
  image_id: '',
  similarity: 1,
  vector_similarity: 1,
  term_similarity: 1,
  positions: [],
});

const HappyPrince = buildChunk('a2d067519c9e0db1');
const Nightingale = buildChunk('b3e178620da1fc42');

describe('parseCitationIndex', () => {
  it('parses numeric markers as numbers', () => {
    expect(parseCitationIndex('[ID:2]')).toBe(2);
    expect(parseCitationIndex('[3]')).toBe(3);
  });

  it('parses hex chunk-id markers as strings', () => {
    expect(parseCitationIndex('[ID:a2d067519c9e0db1]')).toBe(
      'a2d067519c9e0db1',
    );
  });
});

describe('resolveCitationChunk', () => {
  // The payload ships in retrieval order: Happy Prince ranked first.
  const chunks = [HappyPrince, Nightingale];

  it('resolves a numeric key positionally', () => {
    expect(resolveCitationChunk(chunks, 0)).toBe(HappyPrince);
    expect(resolveCitationChunk(chunks, 1)).toBe(Nightingale);
  });

  it('returns undefined for an out-of-range numeric key', () => {
    expect(resolveCitationChunk(chunks, 5)).toBeUndefined();
  });

  it('resolves a chunk-id key by id, not by citation order', () => {
    // The answer cites Nightingale first; the chunk must still be the one
    // the marker names, not chunks[0] (the mis-citation regression).
    expect(resolveCitationChunk(chunks, 'b3e178620da1fc42')).toBe(Nightingale);
    expect(resolveCitationChunk(chunks, 'a2d067519c9e0db1')).toBe(HappyPrince);
  });

  it('falls back to chunk_id when the id field is absent', () => {
    const legacy = [
      { chunk_id: 'b3e178620da1fc42' } as unknown as IReferenceChunk,
    ];
    expect(resolveCitationChunk(legacy, 'b3e178620da1fc42')).toBe(legacy[0]);
  });

  it('returns undefined for an unknown chunk id instead of guessing positionally', () => {
    expect(resolveCitationChunk(chunks, 'ffffffffffffffff')).toBeUndefined();
  });

  it('resolves ids inside a Record-shaped payload', () => {
    const record = {
      a2d067519c9e0db1: HappyPrince,
      b3e178620da1fc42: Nightingale,
    };
    expect(resolveCitationChunk(record, 'b3e178620da1fc42')).toBe(Nightingale);
  });

  it('tolerates an empty payload', () => {
    expect(resolveCitationChunk(undefined, 0)).toBeUndefined();
    expect(resolveCitationChunk(null, 'a2d067519c9e0db1')).toBeUndefined();
  });
});

describe('buildCitationDisplayMap', () => {
  it('numbers unique markers by first occurrence, whatever their key', () => {
    const map = buildCitationDisplayMap(
      'nightingale [ID:b3e178620da1fc42] then happy prince [ID:a2d067519c9e0db1] again [ID:b3e178620da1fc42]',
    );
    expect(map.get('b3e178620da1fc42')).toBe(0);
    expect(map.get('a2d067519c9e0db1')).toBe(1);
    expect(map.size).toBe(2);
  });
});

describe('normalizeCitationMarkers', () => {
  test('rewrites the full-width brackets OpenAI-family models cite in', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m【ID:1】.')).toBe(
      'The cable run is 50 m[ID:1].',
    );
  });

  test('tolerates the spacing and full-width colon models put inside them', () => {
    expect(normalizeCitationMarkers('a【ID: 1】b【ID：2】c')).toBe(
      'a[ID:1]b[ID:2]c',
    );
  });

  test('rewrites the pre-0.13 ##n$$ form', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m##7$$.')).toBe(
      'The cable run is 50 m[ID:7].',
    );
  });

  test('leaves canonical markers untouched', () => {
    expect(normalizeCitationMarkers('The cable run is 50 m[ID:1].')).toBe(
      'The cable run is 50 m[ID:1].',
    );
  });

  test('leaves ordinary full-width brackets alone', () => {
    // 【】 is regular punctuation in Chinese text; only an ID marker is a citation.
    expect(normalizeCitationMarkers('参见【附录】的说明')).toBe(
      '参见【附录】的说明',
    );
  });

  test('normalised markers are matched by the renderer regex', () => {
    const normalized = normalizeCitationMarkers('a【ID:3】b');
    expect(
      [...normalized.matchAll(citationMarkerReg)].map((m) => m[1]),
    ).toEqual(['3']);
  });
});

describe('parseCitationIndex', () => {
  test('reads a numeric marker as a position', () => {
    expect(parseCitationIndex('[ID:2]')).toBe(2);
  });

  test('keeps a non-numeric marker as a key', () => {
    expect(parseCitationIndex('[ID:a1b2c3d4e5f60718]')).toBe(
      'a1b2c3d4e5f60718',
    );
  });
});
